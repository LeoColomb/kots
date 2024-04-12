package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/containers/image/v5/transports/alltransports"
	"github.com/mholt/archiver/v3"
	imagespecsv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/pkg/archives"
	dockerarchive "github.com/replicatedhq/kots/pkg/docker/archive"
	dockerregistry "github.com/replicatedhq/kots/pkg/docker/registry"
	dockerregistrytypes "github.com/replicatedhq/kots/pkg/docker/registry/types"
	dockertypes "github.com/replicatedhq/kots/pkg/docker/types"
	imagetypes "github.com/replicatedhq/kots/pkg/image/types"
	"github.com/replicatedhq/kots/pkg/imageutil"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"github.com/replicatedhq/kots/pkg/logger"
	kotsv1beta1 "github.com/replicatedhq/kotskinds/apis/kots/v1beta1"
	oras "oras.land/oras-go/v2"
	orasfile "oras.land/oras-go/v2/content/file"
	orasremote "oras.land/oras-go/v2/registry/remote"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"
	orasretry "oras.land/oras-go/v2/registry/remote/retry"
)

const (
	EmbeddedClusterArtifactType = "application/vnd.embeddedcluster.artifact"
	EmbeddedClusterMediaType    = "application/vnd.embeddedcluster.file"
)

var (
	ChartsArtifactRegex   = regexp.MustCompile(`\/embedded-cluster\/(charts\.tar\.gz):`)
	ImagesArtifactRegex   = regexp.MustCompile(`\/embedded-cluster\/(images-.+\.tar):`)
	BinaryArtifactRegex   = regexp.MustCompile(`\/embedded-cluster\/(embedded-cluster-.+):`)
	MetadataArtifactRegex = regexp.MustCompile(`\/embedded-cluster\/(version-metadata\.json):`)
)

func ExtractAppAirgapArchive(archive string, destDir string, excludeImages bool, progressWriter io.Writer) error {
	reader, err := os.Open(archive)
	if err != nil {
		return errors.Wrap(err, "failed to open airgap archive")
	}
	defer reader.Close()

	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return errors.Wrap(err, "failed to get new gzip reader")
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "failed to read tar header")
		}

		if header.Name == "." {
			continue
		}

		if excludeImages && header.Typeflag == tar.TypeDir {
			// Once we hit a directory, the rest of the archive is images.
			break
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		dstFileName := filepath.Join(destDir, header.Name)
		if err := os.MkdirAll(filepath.Dir(dstFileName), 0755); err != nil {
			return errors.Wrap(err, "failed to create path")
		}

		err = func() error {
			WriteProgressLine(progressWriter, fmt.Sprintf("Extracting %s", dstFileName))

			dstFile, err := os.Create(dstFileName)
			if err != nil {
				return errors.Wrap(err, "failed to create file")
			}
			defer dstFile.Close()

			if _, err := io.Copy(dstFile, tarReader); err != nil {
				return errors.Wrap(err, "failed to copy file data")
			}

			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}

func WriteProgressLine(progressWriter io.Writer, line string) {
	fmt.Fprint(progressWriter, fmt.Sprintf("%s\n", line))
}

// CopyAirgapImages pushes images found in the airgap bundle/airgap root to the configured registry.
func CopyAirgapImages(opts imagetypes.ProcessImageOptions, log *logger.CLILogger) (*imagetypes.CopyAirgapImagesResult, error) {
	if opts.AirgapBundle == "" {
		return &imagetypes.CopyAirgapImagesResult{}, nil
	}

	pushOpts := imagetypes.PushImagesOptions{
		Registry: dockerregistrytypes.RegistryOptions{
			Endpoint:  opts.RegistrySettings.Hostname,
			Namespace: opts.RegistrySettings.Namespace,
			Username:  opts.RegistrySettings.Username,
			Password:  opts.RegistrySettings.Password,
		},
		Log:            log,
		ProgressWriter: opts.ReportWriter,
		LogForUI:       true,
	}

	copyResult, err := TagAndPushImagesFromBundle(opts.AirgapBundle, pushOpts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to push images from bundle")
	}

	return &imagetypes.CopyAirgapImagesResult{
		EmbeddedClusterArtifacts: copyResult.EmbeddedClusterArtifacts,
	}, nil
}

func TagAndPushImagesFromBundle(airgapBundle string, options imagetypes.PushImagesOptions) (*imagetypes.CopyAirgapImagesResult, error) {
	airgap, err := kotsutil.FindAirgapMetaInBundle(airgapBundle)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find airgap meta")
	}

	switch airgap.Spec.Format {
	case dockertypes.FormatDockerRegistry:
		extractedBundle, err := os.MkdirTemp("", "extracted-airgap-kots")
		if err != nil {
			return nil, errors.Wrap(err, "failed to create temp dir for unarchived airgap bundle")
		}
		defer os.RemoveAll(extractedBundle)

		tarGz := archiver.TarGz{
			Tar: &archiver.Tar{
				ImplicitTopLevelFolder: false,
			},
		}
		if err := tarGz.Unarchive(airgapBundle, extractedBundle); err != nil {
			return nil, errors.Wrap(err, "falied to unarchive airgap bundle")
		}
		if err := PushImagesFromTempRegistry(extractedBundle, airgap.Spec.SavedImages, options); err != nil {
			return nil, errors.Wrap(err, "failed to push images from docker registry bundle")
		}
	case dockertypes.FormatDockerArchive, "":
		if err := PushImagesFromDockerArchiveBundle(airgapBundle, options); err != nil {
			return nil, errors.Wrap(err, "failed to push images from docker archive bundle")
		}
	default:
		return nil, errors.Errorf("Airgap bundle format '%s' is not supported", airgap.Spec.Format)
	}

	pushEmbeddedArtifactsOpts := imagetypes.PushEmbeddedClusterArtifactsOptions{
		Registry:   options.Registry,
		Tag:        imageutil.SanitizeTag(fmt.Sprintf("%s-%s-%s", airgap.Spec.ChannelID, airgap.Spec.UpdateCursor, airgap.Spec.VersionLabel)),
		HTTPClient: orasretry.DefaultClient,
	}
	pushedArtifacts, err := PushEmbeddedClusterArtifacts(airgapBundle, airgap.Spec.EmbeddedClusterArtifacts, pushEmbeddedArtifactsOpts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to push embedded cluster artifacts")
	}

	result := &imagetypes.CopyAirgapImagesResult{
		EmbeddedClusterArtifacts: pushedArtifacts,
	}

	return result, nil
}

func PushImagesFromTempRegistry(airgapRootDir string, imageList []string, options imagetypes.PushImagesOptions) error {
	imagesDir := filepath.Join(airgapRootDir, "images")
	if _, err := os.Stat(imagesDir); os.IsNotExist(err) {
		// this can either be because images were already pushed from the CLI, or it's a diff airgap bundle with no images
		return nil
	}

	tempRegistry := &dockerregistry.TempRegistry{}
	if err := tempRegistry.Start(imagesDir); err != nil {
		return errors.Wrap(err, "failed to start temp registry")
	}
	defer tempRegistry.Stop()

	imageInfos := make(map[string]*imagetypes.ImageInfo)
	for _, image := range imageList {
		layerInfo := make(map[string]*imagetypes.LayerInfo)
		if options.LogForUI {
			layers, err := tempRegistry.GetImageLayers(image)
			if err != nil {
				return errors.Wrapf(err, "failed to get image layers for %s", image)
			}
			layerInfo, err = layerInfoFromLayers(layers)
			if err != nil {
				return errors.Wrap(err, "failed to get layer info")
			}
		}
		imageInfos[image] = &imagetypes.ImageInfo{
			Format: dockertypes.FormatDockerRegistry,
			Layers: layerInfo,
			Status: "queued",
		}
	}

	reportWriter := options.ProgressWriter
	if options.LogForUI {
		wc := reportWriterWithProgress(imageInfos, options.ProgressWriter)
		reportWriter = wc.(io.Writer)
		defer wc.Write([]byte("+status.flush:\n"))
		defer wc.Close()
	}

	totalImages := len(imageInfos)
	var imageCounter int
	for imageID, imageInfo := range imageInfos {
		srcRef, err := tempRegistry.SrcRef(imageID)
		if err != nil {
			return errors.Wrapf(err, "failed to parse source image %s", imageID)
		}

		destImage, err := imageutil.DestImage(options.Registry, imageID)
		if err != nil {
			return errors.Wrapf(err, "failed to get destination image for %s", imageID)
		}

		if options.KotsadmTag != "" {
			// kotsadm tag is set, change the tag of the kotsadm and kotsadm-migrations images
			imageName := imageutil.GetImageName(destImage)
			if imageName == "kotsadm" || imageName == "kotsadm-migrations" {
				di, err := imageutil.ChangeImageTag(destImage, options.KotsadmTag)
				if err != nil {
					return errors.Wrap(err, "failed to change kotsadm dest image tag")
				}
				destImage = di
			}
		}

		destStr := fmt.Sprintf("docker://%s", destImage)
		destRef, err := alltransports.ParseImageName(destStr)
		if err != nil {
			return errors.Wrapf(err, "failed to parse dest image %s", destStr)
		}

		// copy all architecures available in the bundle.
		// this also handles kotsadm airgap bundles that have multi-arch images but are referenced by tag.
		copyAll := true

		pushImageOpts := imagetypes.PushImageOptions{
			ImageID:      imageID,
			ImageInfo:    imageInfo,
			Log:          options.Log,
			LogForUI:     options.LogForUI,
			ReportWriter: reportWriter,
			CopyImageOptions: imagetypes.CopyImageOptions{
				SrcRef:  srcRef,
				DestRef: destRef,
				DestAuth: imagetypes.RegistryAuth{
					Username: options.Registry.Username,
					Password: options.Registry.Password,
				},
				CopyAll:           copyAll,
				SrcDisableV1Ping:  true,
				SrcSkipTLSVerify:  true,
				DestDisableV1Ping: true,
				DestSkipTLSVerify: true,
				ReportWriter:      reportWriter,
			},
		}
		imageCounter++
		fmt.Printf("Pushing application images (%d/%d)\n", imageCounter, totalImages)
		if err := pushImage(pushImageOpts); err != nil {
			return errors.Wrapf(err, "failed to push image %s", imageID)
		}
	}

	return nil
}

func PushImagesFromDockerArchivePath(airgapRootDir string, options imagetypes.PushImagesOptions) error {
	imagesDir := filepath.Join(airgapRootDir, "images")
	if _, err := os.Stat(imagesDir); os.IsNotExist(err) {
		// images were already pushed from the CLI
		return nil
	}

	imageInfos := make(map[string]*imagetypes.ImageInfo)

	walkErr := filepath.Walk(imagesDir,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			layerInfo := make(map[string]*imagetypes.LayerInfo)
			if options.LogForUI {
				layers, err := dockerarchive.GetImageLayers(path)
				if err != nil {
					return errors.Wrap(err, "failed to get image layers")
				}
				layerInfo, err = layerInfoFromLayers(layers)
				if err != nil {
					return errors.Wrap(err, "failed to get layer info")
				}
			}

			imageInfos[path] = &imagetypes.ImageInfo{
				Format: dockertypes.FormatDockerArchive,
				Layers: layerInfo,
				Status: "queued",
			}
			return nil
		})
	if walkErr != nil {
		return errors.Wrap(walkErr, "failed to walk images dir")
	}

	reportWriter := options.ProgressWriter
	if options.LogForUI {
		wc := reportWriterWithProgress(imageInfos, options.ProgressWriter)
		reportWriter = wc.(io.Writer)
		defer wc.Write([]byte("+status.flush:\n"))
		defer wc.Close()
	}

	for imagePath, imageInfo := range imageInfos {
		formatRoot := path.Join(imagesDir, imageInfo.Format)
		pathWithoutRoot := imagePath[len(formatRoot)+1:]
		rewrittenImage, err := imageutil.RewriteDockerArchiveImage(options.Registry, strings.Split(pathWithoutRoot, string(os.PathSeparator)))
		if err != nil {
			return errors.Wrap(err, "failed to rewrite docker archive image")
		}

		srcRef, err := alltransports.ParseImageName(fmt.Sprintf("%s:%s", dockertypes.FormatDockerArchive, imagePath))
		if err != nil {
			return errors.Wrap(err, "failed to parse src image name")
		}

		destStr := fmt.Sprintf("docker://%s", imageutil.DestImageFromKustomizeImage(rewrittenImage))
		destRef, err := alltransports.ParseImageName(destStr)
		if err != nil {
			return errors.Wrapf(err, "failed to parse dest image name %s", destStr)
		}

		pushImageOpts := imagetypes.PushImageOptions{
			ImageID:      imagePath,
			ImageInfo:    imageInfo,
			Log:          options.Log,
			LogForUI:     options.LogForUI,
			ReportWriter: reportWriter,
			CopyImageOptions: imagetypes.CopyImageOptions{
				SrcRef:  srcRef,
				DestRef: destRef,
				DestAuth: imagetypes.RegistryAuth{
					Username: options.Registry.Username,
					Password: options.Registry.Password,
				},
				CopyAll:           false, // docker-archive format does not support multi-arch images
				DestSkipTLSVerify: true,
				DestDisableV1Ping: true,
				ReportWriter:      reportWriter,
			},
		}
		if err := pushImage(pushImageOpts); err != nil {
			return errors.Wrapf(err, "failed to push image %s", imagePath)
		}
	}

	return nil
}

func PushImagesFromDockerArchiveBundle(airgapBundle string, options imagetypes.PushImagesOptions) error {
	if exists, err := archives.DirExistsInAirgap("images", airgapBundle); err != nil {
		return errors.Wrap(err, "failed to check if images dir exists in airgap bundle")
	} else if !exists {
		// images were already pushed from the CLI
		return nil
	}

	if options.LogForUI {
		WriteProgressLine(options.ProgressWriter, "Reading image information from bundle...")
	}

	imageInfos, err := getImageInfosFromBundle(airgapBundle, options.LogForUI)
	if err != nil {
		return errors.Wrap(err, "failed to get images info from bundle")
	}

	fileReader, err := os.Open(airgapBundle)
	if err != nil {
		return errors.Wrap(err, "failed to open file")
	}
	defer fileReader.Close()

	gzipReader, err := gzip.NewReader(fileReader)
	if err != nil {
		return errors.Wrap(err, "failed to get new gzip reader")
	}
	defer gzipReader.Close()

	reportWriter := options.ProgressWriter
	if options.LogForUI {
		wc := reportWriterWithProgress(imageInfos, options.ProgressWriter)
		reportWriter = wc.(io.Writer)
		defer wc.Write([]byte("+status.flush:\n"))
		defer wc.Close()
	}

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "failed to get read archive")
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		imagePath := header.Name
		imageInfo, ok := imageInfos[imagePath]
		if !ok {
			continue
		}

		if options.LogForUI {
			WriteProgressLine(reportWriter, fmt.Sprintf("Extracting image %s", imagePath))
		}

		tmpFile, err := os.CreateTemp("", "kotsadm-image-")
		if err != nil {
			return errors.Wrap(err, "failed to create temp file")
		}
		defer tmpFile.Close()
		defer os.Remove(tmpFile.Name())

		_, err = io.Copy(tmpFile, tarReader)
		if err != nil {
			return errors.Wrapf(err, "failed to write file %q", imagePath)
		}

		// Close file to flush all data before pushing to registry
		if err := tmpFile.Close(); err != nil {
			return errors.Wrap(err, "failed to close tmp file")
		}

		pathParts := strings.Split(imagePath, string(os.PathSeparator))
		if len(pathParts) < 3 {
			return errors.Errorf("not enough path parts in %q", imagePath)
		}

		rewrittenImage, err := imageutil.RewriteDockerArchiveImage(options.Registry, pathParts[2:])
		if err != nil {
			return errors.Wrap(err, "failed to rewrite docker archive image")
		}

		srcRef, err := alltransports.ParseImageName(fmt.Sprintf("%s:%s", dockertypes.FormatDockerArchive, tmpFile.Name()))
		if err != nil {
			return errors.Wrap(err, "failed to parse src image name")
		}

		destStr := fmt.Sprintf("docker://%s", imageutil.DestImageFromKustomizeImage(rewrittenImage))
		destRef, err := alltransports.ParseImageName(destStr)
		if err != nil {
			return errors.Wrapf(err, "failed to parse dest image name %s", destStr)
		}

		pushImageOpts := imagetypes.PushImageOptions{
			ImageID:      imagePath,
			ImageInfo:    imageInfo,
			Log:          options.Log,
			LogForUI:     options.LogForUI,
			ReportWriter: reportWriter,
			CopyImageOptions: imagetypes.CopyImageOptions{
				SrcRef:  srcRef,
				DestRef: destRef,
				DestAuth: imagetypes.RegistryAuth{
					Username: options.Registry.Username,
					Password: options.Registry.Password,
				},
				CopyAll:           false, // docker-archive format does not support multi-arch images
				DestSkipTLSVerify: true,
				DestDisableV1Ping: true,
				ReportWriter:      reportWriter,
			},
		}
		if err := pushImage(pushImageOpts); err != nil {
			return errors.Wrapf(err, "failed to push image %s", imagePath)
		}
	}

	return nil
}

func pushImage(opts imagetypes.PushImageOptions) error {
	opts.ImageInfo.UploadStart = time.Now()
	if opts.LogForUI {
		fmt.Printf("Pushing image %s\n", opts.ImageID) // still log in console for future reference
		opts.ReportWriter.Write([]byte(fmt.Sprintf("+file.begin:%s\n", opts.ImageID)))
	} else {
		destImageStr := opts.CopyImageOptions.DestRef.DockerReference().String() // this is better for debugging from the cli than the image id
		WriteProgressLine(opts.ReportWriter, fmt.Sprintf("Pushing image %s", destImageStr))
	}

	var retryAttempts int = 5
	var copyError error

	for i := 0; i < retryAttempts; i++ {
		copyError = CopyImage(opts.CopyImageOptions)
		if copyError == nil {
			break // image copy succeeded, exit the retry loop
		} else {
			opts.Log.ChildActionWithoutSpinner("encountered error (#%d) copying image, waiting 10s before trying again: %s", i+1, copyError.Error())
			time.Sleep(time.Second * 10)
		}
	}
	if copyError != nil {
		if opts.LogForUI {
			opts.ReportWriter.Write([]byte(fmt.Sprintf("+file.error:%s\n", copyError)))
		}
		opts.Log.FinishChildSpinner()
		return errors.Wrap(copyError, "failed to push image")
	}

	opts.Log.FinishChildSpinner()
	opts.ImageInfo.UploadEnd = time.Now()
	if opts.LogForUI {
		opts.ReportWriter.Write([]byte(fmt.Sprintf("+file.end:%s\n", opts.ImageID)))
	}

	return nil
}

func getImageInfosFromBundle(airgapBundle string, getLayerInfo bool) (map[string]*imagetypes.ImageInfo, error) {
	fileReader, err := os.Open(airgapBundle)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	defer fileReader.Close()

	gzipReader, err := gzip.NewReader(fileReader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get new gzip reader")
	}
	defer gzipReader.Close()

	imageInfos := make(map[string]*imagetypes.ImageInfo)

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "failed to get read archive")
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}
		// check that the file is in the images directory
		pathParts := strings.Split(header.Name, string(os.PathSeparator))
		if len(pathParts) < 2 || pathParts[0] != "images" {
			continue
		}

		layerInfo := make(map[string]*imagetypes.LayerInfo)
		if getLayerInfo {
			layers, err := dockerarchive.GetImageLayersFromReader(tarReader)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get image layers from reader")
			}
			layerInfo, err = layerInfoFromLayers(layers)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get layer info")
			}
		}

		if len(pathParts) < 3 {
			return nil, errors.Errorf("not enough parts in image path: %q", header.Name)
		}

		imageInfos[header.Name] = &imagetypes.ImageInfo{
			Format: dockertypes.FormatDockerArchive,
			Layers: layerInfo,
			Status: "queued",
		}
	}
	return imageInfos, nil
}

func layerInfoFromLayers(layers []dockertypes.Layer) (map[string]*imagetypes.LayerInfo, error) {
	layerInfo := make(map[string]*imagetypes.LayerInfo)
	for _, layer := range layers {
		layerID := strings.TrimPrefix(layer.Digest, "sha256:")
		layerInfo[layerID] = &imagetypes.LayerInfo{
			ID:   layerID,
			Size: layer.Size,
		}
	}
	return layerInfo, nil
}

func reportWriterWithProgress(imageInfos map[string]*imagetypes.ImageInfo, reportWriter io.Writer) io.WriteCloser {
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		currentLayerID := ""
		currentImageID := ""
		currentLine := ""

		scanner := bufio.NewScanner(pipeReader)
		for scanner.Scan() {
			line := scanner.Text()
			// Example sequence of messages we get per image
			//
			// Copying blob sha256:67cddc63a0c4a6dd25d2c7789f7b7cdd9ce1a5d05a0607303c0ef625d0b76d08
			// Copying blob sha256:5dacd731af1b0386ead06c8b1feff9f65d9e0bdfec032d2cd0bc03690698feda
			// Copying blob sha256:b66a10934ed6942a31f8d0e96b1646fe0cbc7a9e0dd58eb686585d3e2d2edd1b
			// Copying blob sha256:0e401eb4a60a193c933bf80ebeab0ac35ac2592bc7c048d6843efb6b1d2f593a
			// Copying config sha256:043316b7542bc66eb4dad30afb998086714862c863f0f267467385fada943681
			// Writing manifest to image destination
			// Storing signatures

			if strings.HasPrefix(line, "Copying blob sha256:") {
				currentLine = line
				progressLayerEnded(currentImageID, currentLayerID, imageInfos)
				currentLayerID = strings.TrimPrefix(line, "Copying blob sha256:")
				progressLayerStarted(currentImageID, currentLayerID, imageInfos)
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else if strings.HasPrefix(line, "Copying config sha256:") {
				currentLine = line
				progressLayerEnded(currentImageID, currentLayerID, imageInfos)
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else if strings.HasPrefix(line, "+file.begin:") {
				currentImageID = strings.TrimPrefix(line, "+file.begin:")
				progressFileStarted(currentImageID, imageInfos)
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else if strings.HasPrefix(line, "+file.end:") {
				progressFileEnded(currentImageID, imageInfos)
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else if strings.HasPrefix(line, "+file.error:") {
				errorStr := strings.TrimPrefix(line, "+file.error:")
				progressFileFailed(currentImageID, imageInfos, errorStr)
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else if strings.HasPrefix(line, "+status.flush:") {
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			} else {
				currentLine = line
				writeCurrentProgress(currentLine, imageInfos, reportWriter)
				continue
			}
		}
	}()

	return pipeWriter
}

func PushEmbeddedClusterArtifacts(airgapBundle string, bundleArtifacts *kotsv1beta1.EmbeddedClusterArtifacts, opts imagetypes.PushEmbeddedClusterArtifactsOptions) (*kotsv1beta1.EmbeddedClusterArtifacts, error) {
	tmpDir, err := os.MkdirTemp("", "embedded-cluster-artifacts")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp directory")
	}
	defer os.RemoveAll(tmpDir)

	fileReader, err := os.Open(airgapBundle)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	defer fileReader.Close()

	gzipReader, err := gzip.NewReader(fileReader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get new gzip reader")
	}
	defer gzipReader.Close()

	// store extracted artifacts for progress reporting
	extractedArtifacts := make(map[string]string)

	// store pushed artifact bundle source and oci destination
	pushedArtifacts := make(map[string]string)
	if bundleArtifacts != nil {
		pushedArtifacts[bundleArtifacts.Binary] = ""
		pushedArtifacts[bundleArtifacts.Charts] = ""
		pushedArtifacts[bundleArtifacts.Images] = ""
		pushedArtifacts[bundleArtifacts.Metadata] = ""
	}

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "failed to get read archive")
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		if bundleArtifacts == nil {
			// if embeddedClusterArtifacts is not in the airgap metadata, we push everything in the "embedded-cluster" directory
			if filepath.Dir(header.Name) != "embedded-cluster" {
				continue
			}
		} else {
			// if embeddedClusterArtifacts is not nil, we only push the files specified in the embeddedClusterArtifacts
			_, ok := pushedArtifacts[header.Name]
			if !ok {
				continue
			}
		}

		dstFilePath := filepath.Join(tmpDir, header.Name)
		if err := os.MkdirAll(filepath.Dir(dstFilePath), 0755); err != nil {
			return nil, errors.Wrap(err, "failed to create path")
		}

		dstFile, err := os.Create(dstFilePath)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create file")
		}

		if _, err := io.Copy(dstFile, tarReader); err != nil {
			dstFile.Close()
			return nil, errors.Wrap(err, "failed to copy file data")
		}

		dstFile.Close()
		extractedArtifacts[header.Name] = dstFilePath
	}

	artifactCounter := 0
	for airgapSource, dstFilePath := range extractedArtifacts {
		name := filepath.Base(dstFilePath)
		repository := filepath.Join("embedded-cluster", imageutil.SanitizeRepo(name))
		artifactFile := imagetypes.OCIArtifactFile{
			Name:      name,
			Path:      dstFilePath,
			MediaType: EmbeddedClusterMediaType,
		}

		pushOCIArtifactOpts := imagetypes.PushOCIArtifactOptions{
			Files:        []imagetypes.OCIArtifactFile{artifactFile},
			ArtifactType: EmbeddedClusterArtifactType,
			Registry:     opts.Registry,
			Repository:   repository,
			Tag:          opts.Tag,
			HTTPClient:   opts.HTTPClient,
		}

		artifactCounter++
		fmt.Printf("Pushing embedded cluster artifacts (%d/%d)\n", artifactCounter, len(extractedArtifacts))
		artifact := fmt.Sprintf("%s:%s", filepath.Join(opts.Registry.Endpoint, opts.Registry.Namespace, repository), opts.Tag)
		if err := pushOCIArtifact(pushOCIArtifactOpts); err != nil {
			return nil, errors.Wrapf(err, "failed to push oci artifact %s", name)
		}
		pushedArtifacts[airgapSource] = artifact
	}

	embeddedClusterOCIArtifacts, err := embeddedClusterArtifactsFromPushedArtifacts(bundleArtifacts, pushedArtifacts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get embedded cluster artifacts from pushed artifacts")
	}

	return embeddedClusterOCIArtifacts, nil
}

func embeddedClusterArtifactsFromPushedArtifacts(bundleArtifacts *kotsv1beta1.EmbeddedClusterArtifacts, pushedArtifacts map[string]string) (*kotsv1beta1.EmbeddedClusterArtifacts, error) {
	if len(pushedArtifacts) == 0 {
		return nil, nil
	}

	embeddedClusterOCIArtifacts := &kotsv1beta1.EmbeddedClusterArtifacts{}
	if bundleArtifacts != nil {
		// validate that all expected embedded cluster artifacts were found in the bundle and pushed
		for airgapSource, ociDestination := range pushedArtifacts {
			if ociDestination == "" {
				return nil, fmt.Errorf("expected embedded cluster artifact %s was not pushed from the airgap bundle", airgapSource)
			}
		}

		embeddedClusterOCIArtifacts.Binary = pushedArtifacts[bundleArtifacts.Binary]
		embeddedClusterOCIArtifacts.Charts = pushedArtifacts[bundleArtifacts.Charts]
		embeddedClusterOCIArtifacts.Images = pushedArtifacts[bundleArtifacts.Images]
		embeddedClusterOCIArtifacts.Metadata = pushedArtifacts[bundleArtifacts.Metadata]
	} else {
		embeddedClusterOCIArtifacts = &kotsv1beta1.EmbeddedClusterArtifacts{}
		for _, ociDestination := range pushedArtifacts {
			switch {
			case BinaryArtifactRegex.MatchString(ociDestination):
				embeddedClusterOCIArtifacts.Binary = ociDestination
			case ChartsArtifactRegex.MatchString(ociDestination):
				embeddedClusterOCIArtifacts.Charts = ociDestination
			case ImagesArtifactRegex.MatchString(ociDestination):
				embeddedClusterOCIArtifacts.Images = ociDestination
			case MetadataArtifactRegex.MatchString(ociDestination):
				embeddedClusterOCIArtifacts.Metadata = ociDestination
			}
		}
	}

	return embeddedClusterOCIArtifacts, nil
}

func pushOCIArtifact(opts imagetypes.PushOCIArtifactOptions) error {
	orasWorkspace, err := os.MkdirTemp("", "oras")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory")
	}
	defer os.RemoveAll(orasWorkspace)

	orasFS, err := orasfile.New(orasWorkspace)
	if err != nil {
		return errors.Wrap(err, "failed to create oras file store")
	}
	defer orasFS.Close()

	fileDescriptors := make([]imagespecsv1.Descriptor, 0)
	for _, f := range opts.Files {
		fileDescriptor, err := orasFS.Add(context.TODO(), f.Name, f.MediaType, f.Path)
		if err != nil {
			return errors.Wrapf(err, "failed to add file %s to oras file store", f.Name)
		}
		fileDescriptors = append(fileDescriptors, fileDescriptor)
	}

	packOpts := oras.PackManifestOptions{
		Layers: fileDescriptors,
	}
	manifestDescriptor, err := oras.PackManifest(context.TODO(), orasFS, oras.PackManifestVersion1_1_RC4, opts.ArtifactType, packOpts)
	if err != nil {
		return errors.Wrap(err, "failed to pack manifest")
	}

	if err = orasFS.Tag(context.TODO(), manifestDescriptor, opts.Tag); err != nil {
		return errors.Wrap(err, "failed to tag manifest")
	}

	repository, err := orasremote.NewRepository(filepath.Join(opts.Registry.Endpoint, opts.Registry.Namespace, opts.Repository))
	if err != nil {
		return errors.Wrap(err, "failed to create remote repository")
	}
	repository.Client = &orasauth.Client{
		Client: opts.HTTPClient,
		Cache:  orasauth.DefaultCache,
		Credential: orasauth.StaticCredential(opts.Registry.Endpoint, orasauth.Credential{
			Username: opts.Registry.Username,
			Password: opts.Registry.Password,
		}),
	}
	repository.PlainHTTP = true

	_, err = oras.Copy(context.TODO(), orasFS, opts.Tag, repository, opts.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return errors.Wrap(err, "failed to copy")
	}

	return nil
}

type ProgressReport struct {
	// set to "progressReport"
	Type string `json:"type"`
	// the same progress text that used to be sent in unstructured message
	CompatibilityMessage string `json:"compatibilityMessage"`
	// all images found in archive
	Images []ProgressImage `json:"images"`
}

type ProgressImage struct {
	// image name and tag, "nginx:latest"
	DisplayName string `json:"displayName"`
	// image upload status: queued, uploading, uploaded, failed
	Status string `json:"status"`
	// error string set when status is failed
	Error string `json:"error"`
	// amount currently uploaded (currently number of layers)
	Current int64 `json:"current"`
	// total amount that needs to be uploaded (currently number of layers)
	Total int64 `json:"total"`
	// time when image started uploading
	StartTime time.Time `json:"startTime"`
	// time when image finished uploading
	EndTime time.Time `json:"endTime"`
}

func progressLayerEnded(imageID, layerID string, imageInfos map[string]*imagetypes.ImageInfo) {
	imageInfo := imageInfos[imageID]
	if imageInfo == nil {
		return
	}

	imageInfo.Status = "uploading"

	layer := imageInfo.Layers[layerID]
	if layer == nil {
		return
	}

	layer.UploadEnd = time.Now()
}

func progressLayerStarted(imageID, layerID string, imageInfos map[string]*imagetypes.ImageInfo) {
	imageInfo := imageInfos[imageID]
	if imageInfo == nil {
		return
	}

	imageInfo.Status = "uploading"

	layer := imageInfo.Layers[layerID]
	if layer == nil {
		return
	}

	layer.UploadStart = time.Now()
}

func progressFileStarted(imageID string, imageInfos map[string]*imagetypes.ImageInfo) {
	imageInfo := imageInfos[imageID]
	if imageInfo == nil {
		return
	}

	imageInfo.Status = "uploading"
	imageInfo.UploadStart = time.Now()
}

func progressFileEnded(imageID string, imageInfos map[string]*imagetypes.ImageInfo) {
	imageInfo := imageInfos[imageID]
	if imageInfo == nil {
		return
	}

	imageInfo.Status = "uploaded"
	imageInfo.UploadEnd = time.Now()
}

func progressFileFailed(imageID string, imageInfos map[string]*imagetypes.ImageInfo, errorStr string) {
	imageInfo := imageInfos[imageID]
	if imageInfo == nil {
		return
	}

	imageInfo.Status = "failed"
	imageInfo.Error = errorStr
	imageInfo.UploadEnd = time.Now()
}

func writeCurrentProgress(line string, imageInfos map[string]*imagetypes.ImageInfo, reportWriter io.Writer) {
	report := ProgressReport{
		Type:                 "progressReport",
		CompatibilityMessage: line,
	}

	images := make([]ProgressImage, 0)
	for id, imageInfo := range imageInfos {
		displayName := ""
		if imageInfo.Format == dockertypes.FormatDockerArchive {
			displayName = pathToDisplayName(id)
		} else {
			displayName = id
		}
		progressImage := ProgressImage{
			DisplayName: displayName,
			Status:      imageInfo.Status,
			Error:       imageInfo.Error,
			Current:     countLayersUploaded(imageInfo),
			Total:       int64(len(imageInfo.Layers)),
			StartTime:   imageInfo.UploadStart,
			EndTime:     imageInfo.UploadEnd,
		}
		images = append(images, progressImage)
	}
	report.Images = images
	data, _ := json.Marshal(report)
	fmt.Fprintf(reportWriter, "%s\n", data)
}

func pathToDisplayName(path string) string {
	tag := filepath.Base(path)
	image := filepath.Base(filepath.Dir(path))
	return image + ":" + tag // TODO: support for SHAs
}

func countLayersUploaded(image *imagetypes.ImageInfo) int64 {
	count := int64(0)
	for _, layer := range image.Layers {
		if !layer.UploadEnd.IsZero() {
			count += 1
		}
	}
	return count
}
