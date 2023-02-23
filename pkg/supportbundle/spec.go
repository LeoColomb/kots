package supportbundle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	"github.com/replicatedhq/kots/kotskinds/client/kotsclientset/scheme"
	apptypes "github.com/replicatedhq/kots/pkg/app/types"
	"github.com/replicatedhq/kots/pkg/helm"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	kotstypes "github.com/replicatedhq/kots/pkg/kotsadm/types"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"github.com/replicatedhq/kots/pkg/kurl"
	"github.com/replicatedhq/kots/pkg/logger"
	"github.com/replicatedhq/kots/pkg/registry"
	registrytypes "github.com/replicatedhq/kots/pkg/registry/types"
	"github.com/replicatedhq/kots/pkg/render/helper"
	"github.com/replicatedhq/kots/pkg/reporting"
	"github.com/replicatedhq/kots/pkg/snapshot"
	kotssnapshot "github.com/replicatedhq/kots/pkg/snapshot"
	"github.com/replicatedhq/kots/pkg/store"
	"github.com/replicatedhq/kots/pkg/supportbundle/defaultspec"
	"github.com/replicatedhq/kots/pkg/supportbundle/types"
	"github.com/replicatedhq/kots/pkg/util"
	troubleshootv1beta2 "github.com/replicatedhq/troubleshoot/pkg/apis/troubleshoot/v1beta2"
	sb "github.com/replicatedhq/troubleshoot/pkg/supportbundle"
	"go.uber.org/multierr"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

var appNameRE *regexp.Regexp

func init() {
	appNameRE = regexp.MustCompile(`^kotsadm-.*-supportbundle$`)
}

// CreateRenderedSpec creates the support bundle specification from defaults and the kots app
func CreateRenderedSpec(app apptypes.AppType, sequence int64, kotsKinds *kotsutil.KotsKinds, opts types.TroubleshootOptions) (*troubleshootv1beta2.SupportBundle, error) {
	builtBundle := kotsKinds.SupportBundle.DeepCopy()
	if builtBundle == nil {
		builtBundle = &troubleshootv1beta2.SupportBundle{
			TypeMeta: v1.TypeMeta{
				Kind:       "SupportBundle",
				APIVersion: "troubleshoot.sh/v1beta2",
			},
			ObjectMeta: v1.ObjectMeta{
				Name: "default-supportbundle",
			},
		}

		if kotsKinds.Collector != nil {
			builtBundle.Spec.Collectors = kotsKinds.Collector.DeepCopy().Spec.Collectors
		}
		if kotsKinds.Analyzer != nil {
			builtBundle.Spec.Analyzers = kotsKinds.Analyzer.DeepCopy().Spec.Analyzers
		}
	}

	clientset, err := k8sutil.GetClientset()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s clientset")
	}

	namespacesToCollect := []string{}
	namespacesToAnalyze := []string{}

	isKurl, err := kurl.IsKurl(clientset)
	if err != nil {
		return nil, errors.Wrap(err, "failed to check if cluster is kurl")
	}

	if !isKurl {
		// with cluster access, collect everything, but only analyze application namespaces
		// with minimal RBAC collect only application namespaces
		if k8sutil.IsKotsadmClusterScoped(context.TODO(), clientset, util.PodNamespace) {
			namespacesToAnalyze = append(namespacesToAnalyze, util.PodNamespace)
			namespacesToAnalyze = append(namespacesToAnalyze, kotsKinds.KotsApplication.Spec.AdditionalNamespaces...)
			veleroNamespace, err := kotssnapshot.DetectVeleroNamespace(context.TODO(), clientset, util.PodNamespace)
			if err != nil {
				logger.Errorf("Failed to detect velero namespace for the support bundle: %v", err)
			} else {
				namespacesToAnalyze = append(namespacesToAnalyze, veleroNamespace)
			}
		} else {
			namespacesToCollect = append(namespacesToCollect, util.PodNamespace)
			namespacesToCollect = append(namespacesToCollect, kotsKinds.KotsApplication.Spec.AdditionalNamespaces...)
		}
	}

	// split the default kotsadm support bundle into multiple support bundles
	builtBundles, err := injectDefaults(app, builtBundle, opts, namespacesToCollect, namespacesToAnalyze)

	if err != nil {
		return nil, errors.Wrap(err, "failed to inject defaults")
	}

	for key, builtBundle := range builtBundles {
		s := serializer.NewYAMLSerializer(serializer.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
		var b bytes.Buffer
		if err := s.Encode(builtBundle, &b); err != nil {
			return nil, errors.Wrap(err, "failed to encode support bundle")
		}

		templatedSpec := b.Bytes()

		renderedSpec, err := helper.RenderAppFile(app, &sequence, templatedSpec, kotsKinds, util.PodNamespace)
		if err != nil {
			return nil, errors.Wrap(err, "failed render support bundle spec")
		}

		// unmarshal the spec, look for image replacements in collectors and then remarshal
		// we do this after template rendering to support templating and then replacement
		supportBundle, err := kotsutil.LoadSupportBundleFromContents(renderedSpec)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal rendered support bundle spec")
		}

		var registrySettings registrytypes.RegistrySettings
		if !util.IsHelmManaged() {
			s, err := store.GetStore().GetRegistryDetailsForApp(app.GetID())
			if err != nil {
				return nil, errors.Wrap(err, "failed to get registry settings for app")
			}
			registrySettings = s
		}

		collectors, err := registry.UpdateCollectorSpecsWithRegistryData(supportBundle.Spec.Collectors, registrySettings, kotsKinds.Installation.Spec.KnownImages, kotsKinds.License, &kotsKinds.KotsApplication)
		if err != nil {
			return nil, errors.Wrap(err, "failed to update collectors")
		}
		supportBundle.Spec.Collectors = collectors
		b.Reset()
		if err := s.Encode(supportBundle, &b); err != nil {
			return nil, errors.Wrap(err, "failed to encode support bundle")
		}
		renderedSpec = b.Bytes()
		secretName := GetSpecSecretName(app.GetSlug())
		if key != "kotsadm" {
			secretName = GetSpecSecretName(app.GetSlug() + "-" + key)
		}

		existingSecret, err := clientset.CoreV1().Secrets(util.PodNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
		labels := kotstypes.MergeLabels(kotstypes.GetKotsadmLabels(), kotstypes.GetTroubleshootLabels())
		if err != nil {
			if kuberneteserrors.IsNotFound(err) {
				secret := &corev1.Secret{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       "Secret",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      secretName,
						Namespace: util.PodNamespace,
						Labels:    labels,
					},
					Data: map[string][]byte{
						SpecDataKey: renderedSpec,
					},
				}

				_, err = clientset.CoreV1().Secrets(util.PodNamespace).Create(context.TODO(), secret, metav1.CreateOptions{})
				if err != nil {
					return nil, errors.Wrap(err, "failed to create support bundle secret")
				}

				logger.Debugf("created %q default support bundle spec secret", app.GetSlug())
				// return supportBundle, nil
			} else {
				return nil, errors.Wrap(err, "failed to read support bundle secret")
			}
		} else {
			if existingSecret.Data == nil {
				existingSecret.Data = map[string][]byte{}
			}
			existingSecret.Data[SpecDataKey] = renderedSpec
			existingSecret.ObjectMeta.Labels = labels

			_, err = clientset.CoreV1().Secrets(util.PodNamespace).Update(context.TODO(), existingSecret, metav1.UpdateOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "failed to update support bundle secret")
			}
		}
	}

	// Include discovered support bundle specs and default kotsadm support bundle spec. Perform this action here so
	// as not to add discovered specs to the default support bundle spec secret.c
	return addDiscoveredSpecs(builtBundles["kotsadm"], app, clientset), nil
}

// addClusterSpecificSpec adds cluster specific and upload results URI to the support bundle
func addClusterSpecificSpec(app apptypes.AppType, b *troubleshootv1beta2.SupportBundle, opts types.TroubleshootOptions, namespacesToCollect []string, namespacesToAnalyze []string, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	supportBundle := b.DeepCopy()

	// determine an upload URL
	var uploadURL string
	var redactURL string
	randomBundleID := strings.ToLower(rand.String(32))
	if opts.DisableUpload {
		//Just use the library internally
		return supportBundle
	} else if opts.Origin != "" {
		uploadURL = fmt.Sprintf("%s/api/v1/troubleshoot/%s/%s", opts.Origin, app.GetID(), randomBundleID)
		redactURL = fmt.Sprintf("%s/api/v1/troubleshoot/supportbundle/%s/redactions", opts.Origin, randomBundleID)
	} else if opts.InCluster {
		uploadURL = fmt.Sprintf("%s/api/v1/troubleshoot/%s/%s", fmt.Sprintf("http://kotsadm.%s.svc.cluster.local:3000", util.PodNamespace), app.GetID(), randomBundleID)
		redactURL = fmt.Sprintf("%s/api/v1/troubleshoot/supportbundle/%s/redactions", fmt.Sprintf("http://kotsadm.%s.svc.cluster.local:3000", util.PodNamespace), randomBundleID)
	} else {
		uploadURL = fmt.Sprintf("%s/api/v1/troubleshoot/%s/%s", os.Getenv("API_ADVERTISE_ENDPOINT"), app.GetID(), randomBundleID)
		redactURL = fmt.Sprintf("%s/api/v1/troubleshoot/supportbundle/%s/redactions", os.Getenv("API_ADVERTISE_ENDPOINT"), randomBundleID)
	}

	supportBundle.Spec.AfterCollection = []*troubleshootv1beta2.AfterCollection{
		{
			UploadResultsTo: &troubleshootv1beta2.ResultRequest{
				URI:       uploadURL,
				Method:    "PUT",
				RedactURI: redactURL,
			},
		},
	}

	supportBundle = addDefaultDynamicTroubleshoot(supportBundle, app, imageName, pullSecret)
	supportBundle = populateNamespaces(supportBundle, namespacesToCollect, namespacesToAnalyze)
	supportBundle = deduplicatedCollectors(supportBundle)
	supportBundle = deduplicatedAnalyzers(supportBundle)

	return supportBundle
}

// createClusterSpecificSupportBundle creates a support bundle spec with only cluster specific collectors, analyzers and upload result URI.
func createClusterSpecificSupportBundle(app apptypes.AppType, b *troubleshootv1beta2.SupportBundle, opts types.TroubleshootOptions, namespacesToCollect []string, namespacesToAnalyze []string, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	supportBundle := &troubleshootv1beta2.SupportBundle{
		TypeMeta: v1.TypeMeta{
			Kind:       "SupportBundle",
			APIVersion: "troubleshoot.sh/v1beta2",
		},
		ObjectMeta: v1.ObjectMeta{
			Name: "cluster-specific-supportbundle",
		},
	}

	supportBundle = addClusterSpecificSpec(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)

	return supportBundle
}

// addDefaultSpec adds the default collectors and analyzers to the support bundle spec.
func addDefaultSpec(app apptypes.AppType, b *troubleshootv1beta2.SupportBundle, opts types.TroubleshootOptions, namespacesToCollect []string, namespacesToAnalyze []string, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	supportBundle := b.DeepCopy()

	supportBundle = addDefaultTroubleshoot(supportBundle, imageName, pullSecret)
	supportBundle = populateNamespaces(supportBundle, namespacesToCollect, namespacesToAnalyze)
	supportBundle = deduplicatedCollectors(supportBundle)
	supportBundle = deduplicatedAnalyzers(supportBundle)

	return supportBundle
}

// createDefaultSupportBundle creates a support bundle spec with only default collectors and analyzers.
func createDefaultSupportBundle(app apptypes.AppType, b *troubleshootv1beta2.SupportBundle, opts types.TroubleshootOptions, namespacesToCollect []string, namespacesToAnalyze []string, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	supportBundle := &troubleshootv1beta2.SupportBundle{
		TypeMeta: v1.TypeMeta{
			Kind:       "SupportBundle",
			APIVersion: "troubleshoot.sh/v1beta2",
		},
		ObjectMeta: v1.ObjectMeta{
			Name: "cluster-specific-supportbundle",
		},
	}

	supportBundle = addDefaultSpec(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)

	return supportBundle
}

// injectDefaults injects the kotsadm default collectors/analyzers in the the support bundle specification and split into multiple specs.
func injectDefaults(app apptypes.AppType, b *troubleshootv1beta2.SupportBundle, opts types.TroubleshootOptions, namespacesToCollect []string, namespacesToAnalyze []string) (map[string]*troubleshootv1beta2.SupportBundle, error) {
	supportBundle := b.DeepCopy()

	clientset, err := k8sutil.GetClientset()
	if err != nil {
		logger.Errorf("Failed to get kubernetes clientset: %v", err)
		return nil, err
	}

	var imageName string
	var pullSecret *troubleshootv1beta2.ImagePullSecrets
	if clientset != nil {
		imageName, pullSecret, err = getImageAndSecret(context.TODO(), clientset)
		if err != nil {
			logger.Errorf("Failed to get kotsadm image and secret: %v", err)
			return nil, err
		}
	}

	if supportBundle == nil {
		supportBundle = &troubleshootv1beta2.SupportBundle{}
	}
	if supportBundle.Spec.Collectors == nil {
		supportBundle.Spec.Collectors = make([]*troubleshootv1beta2.Collect, 0)
	}
	if supportBundle.Spec.Analyzers == nil {
		supportBundle.Spec.Analyzers = make([]*troubleshootv1beta2.Analyze, 0)
	}

	venderSupportBundle := supportBundle.DeepCopy()
	clusterSpecificSupportBundle := createClusterSpecificSupportBundle(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)
	defaultSupportBundle := createDefaultSupportBundle(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)

	supportBundle = addClusterSpecificSpec(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)
	supportBundle = addDefaultSpec(app, supportBundle, opts, namespacesToCollect, namespacesToAnalyze, imageName, pullSecret)

	return map[string]*troubleshootv1beta2.SupportBundle{
		"vender":           venderSupportBundle,          //vendors' application support-bundle spec
		"cluster-specific": clusterSpecificSupportBundle, //cluster-specific support-bundle spec with upload results URI
		"default":          defaultSupportBundle,         //default support-bundle spec
		"kotsadm":          supportBundle,                //default kotsadm support-bundle spec (backward compatible)
	}, nil
}

func addDiscoveredSpecs(
	supportBundle *troubleshootv1beta2.SupportBundle, app apptypes.AppType, clientset kubernetes.Interface,
) *troubleshootv1beta2.SupportBundle {
	specs, err := findSupportBundleSpecs(clientset)
	if err != nil {
		logger.Errorf("Failed to find support bundle secrets: %v", err)
		return supportBundle
	}

	for _, specData := range specs {
		sbObject, err := sb.ParseSupportBundleFromDoc([]byte(specData))
		if err != nil {
			logger.Errorf("Failed to unmarshal support bundle spec: %v", err)
			continue
		}

		// ParseSupportBundleFromDoc will check if there is a uri field and if so,
		// use the upstream spec, otherwise fall back to
		// what's defined in the current spec
		supportBundle.Spec.Collectors = append(supportBundle.Spec.Collectors, sbObject.Spec.Collectors...)
		supportBundle.Spec.Analyzers = append(supportBundle.Spec.Analyzers, sbObject.Spec.Analyzers...)
	}

	// remove duplicated collectors and analyzers if there are multiple support bundle upstream spec
	supportBundle = deduplicatedCollectors(supportBundle)
	supportBundle = deduplicatedAnalyzers(supportBundle)

	return supportBundle
}

// findSupportBundleSpecs finds all support bundle secrets/configmaps in the cluster
// The function will query all objects with troubleshoot.io/kind=support-bundle label
// and, in code, filter out all kotsadm objects that have an object name
// following kotsadm-<app-slug>-supportbundle format.
// Reference: https://troubleshoot.sh/docs/support-bundle/discover-cluster-specs/
func findSupportBundleSpecs(client kubernetes.Interface) ([]string, error) {
	labelSelector := kotstypes.TroubleshootKey + "=" + kotstypes.TroubleshootValue
	ctx := context.TODO()

	specs := []string{}

	// Get all namespaces
	namespaces := []string{}
	if k8sutil.IsKotsadmClusterScoped(ctx, client, util.PodNamespace) {
		nsList, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, ns := range nsList.Items {
			namespaces = append(namespaces, ns.Name)
		}
	} else {
		namespaces = []string{util.PodNamespace}
	}

	// List objects from one namespace at a time so as to isolate errors e.g RBAC
	// Search secrets
	for _, ns := range namespaces {
		secrets, err := client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			logger.Errorf("failed to list secrets in namespace %q: %v", ns, err)
			continue
		}

		for _, obj := range secrets.Items {
			// Filter out all kotsadm objects
			if appNameRE.MatchString(obj.Name) {
				continue
			}

			if obj.Data == nil {
				continue
			}

			specData, ok := obj.Data[SpecDataKey]
			if !ok {
				continue
			}

			specs = append(specs, string(specData))
		}
	}

	// Search config maps
	for _, ns := range namespaces {
		configmaps, err := client.CoreV1().ConfigMaps(ns).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			logger.Errorf("failed to list configmaps in namespace %q: %v", ns, err)
			continue
		}

		for _, obj := range configmaps.Items {
			// Filter out all kotsadm objects
			if appNameRE.MatchString(obj.Name) {
				continue
			}

			if obj.Data == nil {
				continue
			}

			specData, ok := obj.Data[SpecDataKey]
			if !ok {
				continue
			}

			specs = append(specs, specData)
		}
	}

	logger.Debugf("Discovered %d support bundle specs", len(specs))
	return specs, nil
}

// if a namespace is not set for a secret/run/logs/exec/copy collector, set it to the current namespace
// if kotsadm is running with minimal rbac priviliges, only collect resources from the specified minimal rbac namespaces
func populateNamespaces(supportBundle *troubleshootv1beta2.SupportBundle, namespacesToCollect []string, namespacesToAnalyze []string) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()

	// collectors
	var collects []*troubleshootv1beta2.Collect
	for _, collect := range next.Spec.Collectors {
		if collect.Secret != nil && collect.Secret.Namespace == "" {
			collect.Secret.Namespace = util.PodNamespace
		}
		if collect.ConfigMap != nil && collect.ConfigMap.Namespace == "" {
			collect.ConfigMap.Namespace = util.PodNamespace
		}
		if collect.Run != nil && collect.Run.Namespace == "" {
			collect.Run.Namespace = util.PodNamespace
		}
		if collect.Logs != nil && collect.Logs.Namespace == "" {
			collect.Logs.Namespace = util.PodNamespace
		}
		if collect.Exec != nil && collect.Exec.Namespace == "" {
			collect.Exec.Namespace = util.PodNamespace
		}
		if collect.Copy != nil && collect.Copy.Namespace == "" {
			collect.Copy.Namespace = util.PodNamespace
		}
		if len(namespacesToCollect) > 0 {
			if collect.ClusterResources != nil && len(collect.ClusterResources.Namespaces) == 0 {
				collect.ClusterResources.Namespaces = namespacesToCollect
			}
		}
		collects = append(collects, collect)
	}
	next.Spec.Collectors = collects

	// analyzers
	if len(namespacesToAnalyze) > 0 {
		isEmpty := func(namespace string, namespaces []string) bool {
			if len(namespace) > 0 {
				return false
			}
			if len(namespaces) > 0 {
				return false
			}
			return true
		}

		var analyzers []*troubleshootv1beta2.Analyze
		for _, analyzer := range next.Spec.Analyzers {

			if analyzer.ClusterPodStatuses != nil && isEmpty("", analyzer.ClusterPodStatuses.Namespaces) {
				analyzer.ClusterPodStatuses.Namespaces = namespacesToAnalyze
			}

			if analyzer.DeploymentStatus != nil && isEmpty(analyzer.DeploymentStatus.Namespace, analyzer.DeploymentStatus.Namespaces) {
				analyzer.DeploymentStatus.Namespaces = namespacesToAnalyze
			}

			if analyzer.JobStatus != nil && isEmpty(analyzer.JobStatus.Namespace, analyzer.JobStatus.Namespaces) {
				analyzer.JobStatus.Namespaces = namespacesToAnalyze
			}

			if analyzer.ReplicaSetStatus != nil && isEmpty(analyzer.ReplicaSetStatus.Namespace, analyzer.ReplicaSetStatus.Namespaces) {
				analyzer.ReplicaSetStatus.Namespaces = namespacesToAnalyze
			}

			if analyzer.StatefulsetStatus != nil && isEmpty(analyzer.StatefulsetStatus.Namespace, analyzer.StatefulsetStatus.Namespaces) {
				analyzer.StatefulsetStatus.Namespaces = namespacesToAnalyze
			}

			analyzers = append(analyzers, analyzer)
		}
		next.Spec.Analyzers = analyzers
	}

	return next
}

func deduplicatedCollectors(supportBundle *troubleshootv1beta2.SupportBundle) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()

	collectors := []*troubleshootv1beta2.Collect{}

	hasClusterResources := false
	hasClusterInfo := false
	hasCeph := false
	hasLonghorn := false
	hasSysctl := false

	for _, c := range next.Spec.Collectors {
		if c.ClusterResources != nil {
			if hasClusterResources {
				continue
			}
			hasClusterResources = true
		}

		if c.ClusterInfo != nil {
			if hasClusterInfo {
				continue
			}
			hasClusterInfo = true
		}

		if c.Ceph != nil {
			if hasCeph {
				continue
			}
			hasCeph = true
		}

		if c.Longhorn != nil {
			if hasLonghorn {
				continue
			}
			hasLonghorn = true
		}

		if c.Sysctl != nil {
			if hasSysctl {
				continue
			}
			hasSysctl = true
		}

		collectors = append(collectors, c)
	}

	next.Spec.Collectors = collectors

	return next
}

func deduplicatedAnalyzers(supportBundle *troubleshootv1beta2.SupportBundle) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()

	analyzers := []*troubleshootv1beta2.Analyze{}

	hasClusterVersion := false
	hasLonghorn := false
	hasWeaveReport := false

	for _, a := range next.Spec.Analyzers {
		if a.ClusterVersion != nil {
			if hasClusterVersion {
				continue
			}
			hasClusterVersion = true
		}

		if a.Longhorn != nil {
			if hasLonghorn {
				continue
			}
			hasLonghorn = true
		}

		if a.WeaveReport != nil {
			if hasWeaveReport {
				continue
			}
			hasWeaveReport = true
		}

		analyzers = append(analyzers, a)
	}

	next.Spec.Analyzers = analyzers

	return next
}

// addDefaultTroubleshoot adds kots.io (github.com/replicatedhq/kots/support-bundle/spec.yaml) spec to the support bundle.
func addDefaultTroubleshoot(supportBundle *troubleshootv1beta2.SupportBundle, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	clientset, err := k8sutil.GetClientset()
	if err != nil {
		logger.Errorf("Failed to get kubernetes clientset: %v", err)
	}

	isKurl, err := kurl.IsKurl(clientset)
	if err != nil {
		logger.Errorf("Failed to check if cluster is kurl: %v", err)
	}
	next := supportBundle.DeepCopy()
	next.Spec.Collectors = append(next.Spec.Collectors, getDefaultCollectors(imageName, pullSecret, isKurl)...)
	next.Spec.Analyzers = append(next.Spec.Analyzers, getDefaultAnalyzers(isKurl)...)
	return next
}

func getDefaultCollectors(imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets, isKurl bool) []*troubleshootv1beta2.Collect {
	supportBundle := defaultspec.Get()
	if imageName != "" {
		supportBundle = *populateImages(&supportBundle, imageName, pullSecret)
	}
	if !isKurl {
		supportBundle = *removeKurlCollectors(&supportBundle)
	}
	return supportBundle.Spec.Collectors
}

func getDefaultAnalyzers(isKurl bool) []*troubleshootv1beta2.Analyze {
	defaultAnalyzers := defaultspec.Get().Spec.Analyzers
	if !isKurl {
		defaultAnalyzers = removeKurlAnalyzers(defaultAnalyzers)
	}
	return defaultAnalyzers
}

// addDefaultDynamicTroubleshoot adds dynamic spec to the support bundle.
// prefer addDefaultTroubleshoot unless absolutely necessary to encourage consistency across built-in and kots.io specs.
func addDefaultDynamicTroubleshoot(supportBundle *troubleshootv1beta2.SupportBundle, app apptypes.AppType, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()
	next.Spec.Collectors = append(next.Spec.Collectors, getDefaultDynamicCollectors(app, imageName, pullSecret)...)
	next.Spec.Analyzers = append(next.Spec.Analyzers, getDefaultDynamicAnalyzers(app)...)
	return next
}

func getDefaultDynamicCollectors(app apptypes.AppType, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) []*troubleshootv1beta2.Collect {
	collectors := make([]*troubleshootv1beta2.Collect, 0)

	var err error
	var license *v1beta1.License
	switch a := app.(type) {
	case *apptypes.App:
		license, err = store.GetStore().GetLatestLicenseForApp(app.GetID())
		if err != nil {
			logger.Errorf("Failed to load license data from store: %v", err)
		}
	case *apptypes.HelmApp:
		license, err = helm.GetChartLicenseFromSecretOrDownload(a)
		if err != nil {
			logger.Errorf("Failed to load license data from helm: %v", err)
		}
	}

	if license != nil {
		s := serializer.NewSerializerWithOptions(serializer.DefaultMetaFactory, scheme.Scheme, scheme.Scheme, serializer.SerializerOptions{Yaml: true, Pretty: false, Strict: false})
		var b bytes.Buffer
		if err := s.Encode(license, &b); err != nil {
			logger.Errorf("Failed to marshal license: %v", err)
		} else {
			collectors = append(collectors, &troubleshootv1beta2.Collect{
				Data: &troubleshootv1beta2.Data{
					CollectorMeta: troubleshootv1beta2.CollectorMeta{
						CollectorName: "license.yaml",
					},
					Name: "kots/admin_console",
					Data: b.String(),
				},
			})
		}
	}

	reportingInfo := reporting.GetReportingInfo(app.GetID())
	if b, err := json.MarshalIndent(reportingInfo, "", "  "); err != nil {
		logger.Errorf("Failed to marshal reporting info: %v", err)
	} else {
		collectors = append(collectors, &troubleshootv1beta2.Collect{
			Data: &troubleshootv1beta2.Data{
				CollectorMeta: troubleshootv1beta2.CollectorMeta{
					CollectorName: "app-info.json",
				},
				Name: "kots/admin_console",
				Data: string(b),
			},
		})
	}

	collectors = append(collectors, &troubleshootv1beta2.Collect{
		Data: &troubleshootv1beta2.Data{
			CollectorMeta: troubleshootv1beta2.CollectorMeta{
				CollectorName: "namespace.txt",
			},
			Name: "kots/admin_console",
			Data: util.PodNamespace,
		},
	})

	collectors = append(collectors, &troubleshootv1beta2.Collect{
		Secret: &troubleshootv1beta2.Secret{
			CollectorMeta: troubleshootv1beta2.CollectorMeta{
				CollectorName: fmt.Sprintf("%s-registry", app.GetSlug()),
			},
			Name:         fmt.Sprintf("%s-registry", app.GetSlug()),
			Namespace:    util.PodNamespace,
			Key:          ".dockerconfigjson",
			IncludeValue: false,
		},
	})

	collectors = append(collectors, makeVeleroCollectors()...)

	if app, ok := app.(*apptypes.App); ok {
		apps := []*apptypes.App{}
		if app != nil {
			apps = append(apps, app)
		} else {
			var err error
			apps, err = store.GetStore().ListInstalledApps()
			if err != nil {
				logger.Errorf("Failed to list installed apps: %v", err)
			}
		}

		if len(apps) > 0 {
			appVersionArchiveCollectors, err := makeAppVersionArchiveCollectors(apps)
			if err != nil {
				logger.Errorf("Failed to make app version archive collectors: %v", err)
			}
			collectors = append(collectors, appVersionArchiveCollectors...)
		}
	}

	clientset, err := k8sutil.GetClientset()
	if err != nil {
		logger.Errorf("Failed to get kubernetes clientset: %v", err)
	}

	isKurl, err := kurl.IsKurl(clientset)
	if err != nil {
		logger.Errorf("Failed to check if cluster is kurl: %v", err)
	}

	if isKurl {
		collectors = append(collectors, &troubleshootv1beta2.Collect{
			Sysctl: &troubleshootv1beta2.Sysctl{
				Image:           imageName,
				ImagePullSecret: pullSecret,
			},
		})
	}

	if license != nil && license.Spec.IsSnapshotSupported {
		fsMinioErrors := snapshot.GetFileSystemMinioErrors(context.TODO(), clientset)
		if len(fsMinioErrors) > 0 {
			data, _ := yaml.Marshal(fsMinioErrors)
			if len(data) > 0 {
				collectors = append(collectors, &troubleshootv1beta2.Collect{
					Data: &troubleshootv1beta2.Data{
						CollectorMeta: troubleshootv1beta2.CollectorMeta{
							CollectorName: "fs-minio-events.yaml",
						},
						Name: "kots/admin_console",
						Data: string(data),
					},
				})
			}
		}
	}

	return collectors
}

func getDefaultDynamicAnalyzers(app apptypes.AppType) []*troubleshootv1beta2.Analyze {
	analyzers := make([]*troubleshootv1beta2.Analyze, 0)
	analyzers = append(analyzers, makeAPIReplicaAnalyzer())

	analyzers = append(analyzers, &troubleshootv1beta2.Analyze{
		TextAnalyze: &troubleshootv1beta2.TextAnalyze{
			AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{
				CheckName: "NFS Client Package",
			},
			IgnoreIfNoFiles: true,
			FileName:        "kots/admin_console/fs-minio-events.yaml",
			RegexPattern:    "bad option; for several filesystems \\(e\\.g\\. nfs, cifs\\) you might need a \\/sbin\\/mount\\..+type.+ helper program\\.",
			Outcomes: []*troubleshootv1beta2.Outcome{
				{
					Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "true",
						Message: "An NFS client package might be missing. Refer to the [documentation](https://docs.replicated.com/enterprise/snapshots-configuring-nfs) on how to configure NFS snapshots.",
						URI:     "https://docs.replicated.com/enterprise/snapshots-configuring-nfs",
					},
				},
				{
					Pass: &troubleshootv1beta2.SingleOutcome{
						When:    "false",
						Message: "No NFS client errors were found.",
					},
				},
			},
		},
	})

	clientset, err := k8sutil.GetClientset()
	if err != nil {
		logger.Errorf("Failed to get kubernetes clientset: %v", err)
	}

	isKurl, err := kurl.IsKurl(clientset)
	if err != nil {
		logger.Errorf("Failed to check if cluster is kurl: %v", err)
	}

	if isKurl {
		analyzers = append(analyzers,
			&troubleshootv1beta2.Analyze{
				Sysctl: &troubleshootv1beta2.SysctlAnalyze{
					AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{
						CheckName: "IP forwarding not enabled",
					},
					Outcomes: []*troubleshootv1beta2.Outcome{
						{
							Fail: &troubleshootv1beta2.SingleOutcome{
								When:    "net.ipv4.ip_forward = 0",
								Message: "IP forwarding not enabled",
							},
						},
					},
				},
			},
			&troubleshootv1beta2.Analyze{
				Sysctl: &troubleshootv1beta2.SysctlAnalyze{
					AnalyzeMeta: troubleshootv1beta2.AnalyzeMeta{
						CheckName: "Bridge iptables integration",
					},
					Outcomes: []*troubleshootv1beta2.Outcome{
						{
							Fail: &troubleshootv1beta2.SingleOutcome{
								When:    "net.bridge.bridge-nf-call-iptables = 0",
								Message: "Packets traversing bridge interfaces not processed by iptables",
							},
						},
					},
				},
			},
		)
	}

	return analyzers
}

func makeVeleroCollectors() []*troubleshootv1beta2.Collect {
	collectors := []*troubleshootv1beta2.Collect{}

	clientset, err := k8sutil.GetClientset()
	if err != nil {
		logger.Error(err)
		return collectors
	}

	veleroNamespace, err := snapshot.DetectVeleroNamespace(context.TODO(), clientset, util.PodNamespace)
	if err != nil {
		logger.Error(err)
		return collectors
	}

	if veleroNamespace == "" {
		return collectors
	}

	selectors := []string{
		"component=velero",
		"app.kubernetes.io/name=velero",
	}

	for _, selector := range selectors {
		collectors = append(collectors, &troubleshootv1beta2.Collect{
			Logs: &troubleshootv1beta2.Logs{
				CollectorMeta: troubleshootv1beta2.CollectorMeta{
					CollectorName: "velero",
				},
				Name:      "velero",
				Selector:  []string{selector},
				Namespace: veleroNamespace,
			},
		})

	}
	return collectors
}

func makeAppVersionArchiveCollectors(apps []*apptypes.App) ([]*troubleshootv1beta2.Collect, error) {
	dirPrefix, err := ioutil.TempDir("", "app-version-archive")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp dir")
	}
	go func() {
		<-time.After(10 * time.Minute)
		_ = os.RemoveAll(dirPrefix)
	}()

	collectors := []*troubleshootv1beta2.Collect{}
	for _, app := range apps {
		collector, err := makeAppVersionArchiveCollector(app, dirPrefix)
		if err != nil {
			err = multierr.Append(err, errors.Wrapf(err, "failed to make collector for app %s", app.Slug))
		} else {
			collectors = append(collectors, collector)
		}
	}

	return collectors, err
}

func makeAppVersionArchiveCollector(app *apptypes.App, dirPrefix string) (*troubleshootv1beta2.Collect, error) {
	fileName := filepath.Join(dirPrefix, fmt.Sprintf("%s.tar", app.Slug))
	archive, err := os.Create(fileName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create temp file %s", fileName)
	}

	latestSequence, err := store.GetStore().GetLatestAppSequence(app.ID, true)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get latest app sequence")
	}

	tempPath, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(tempPath)

	err = store.GetStore().GetAppVersionArchive(app.ID, latestSequence, tempPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get app version archive")
	}

	tarWriter := tar.NewWriter(archive)
	defer tarWriter.Close()

	// only upstream includes files that don't contain any secrets
	archivePath := filepath.Join(tempPath, "upstream")

	err = filepath.Walk(archivePath, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if archivePath == path {
			// root dir itself is the first item in the walk
			return nil
		}

		trimmedPath := strings.TrimPrefix(path, archivePath)

		// do not include userdata in archive
		if filepath.HasPrefix(trimmedPath, "/userdata") {
			return nil
		}

		tarHeader, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return errors.Wrapf(err, "failed to get tar header from file info header for file %s", trimmedPath)
		}
		tarHeader.Name = trimmedPath

		if err := tarWriter.WriteHeader(tarHeader); err != nil {
			return errors.Wrapf(err, "failed to write tar header for file %s", trimmedPath)
		}

		if fi.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return errors.Wrapf(err, "failed to open file %s", trimmedPath)
		}
		defer file.Close()

		if _, err := io.Copy(tarWriter, file); err != nil {
			return errors.Wrapf(err, "failed to write file %s contents", trimmedPath)
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to walk archive dir %s", archivePath)
	}

	return &troubleshootv1beta2.Collect{
		Copy: &troubleshootv1beta2.Copy{
			CollectorMeta: troubleshootv1beta2.CollectorMeta{
				CollectorName: fmt.Sprintf("spec-%s", app.Slug),
			},
			Selector: []string{
				"app=kotsadm", // can we assume this?
			},
			Namespace:     util.PodNamespace,
			ContainerName: "kotsadm", // can we assume this? kotsadm-api
			ContainerPath: fileName,
			Name:          fmt.Sprintf("kots/admin_console/app/%s", app.Slug),
		},
	}, nil
}

func makeAPIReplicaAnalyzer() *troubleshootv1beta2.Analyze {
	if os.Getenv("POD_OWNER_KIND") == "deployment" {
		return &troubleshootv1beta2.Analyze{
			DeploymentStatus: &troubleshootv1beta2.DeploymentStatus{
				Name:      "kotsadm",
				Namespace: util.PodNamespace,
				Outcomes: []*troubleshootv1beta2.Outcome{
					{
						Pass: &troubleshootv1beta2.SingleOutcome{
							When:    "> 0",
							Message: "At least 1 replica of the Admin Console API is running and ready",
						},
					},
					{
						Fail: &troubleshootv1beta2.SingleOutcome{
							When:    "= 0",
							Message: "There are no replicas of the Admin Console API running and ready",
						},
					},
				},
			},
		}
	}
	return &troubleshootv1beta2.Analyze{
		StatefulsetStatus: &troubleshootv1beta2.StatefulsetStatus{
			Name:      "kotsadm",
			Namespace: util.PodNamespace,
			Outcomes: []*troubleshootv1beta2.Outcome{
				{
					Pass: &troubleshootv1beta2.SingleOutcome{
						When:    "> 0",
						Message: "At least 1 replica of the Admin Console API is running and ready",
					},
				},
				{
					Fail: &troubleshootv1beta2.SingleOutcome{
						When:    "= 0",
						Message: "There are no replicas of the Admin Console API running and ready",
					},
				},
			},
		},
	}
}

func getImageAndSecret(ctx context.Context, clientset kubernetes.Interface) (imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets, err error) {
	namespace := util.PodNamespace

	var containers []corev1.Container
	var imagePullSecrets []corev1.LocalObjectReference
	if os.Getenv("POD_OWNER_KIND") == "deployment" {
		existingDeployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), "kotsadm", metav1.GetOptions{})
		if err != nil {
			return imageName, pullSecret, errors.Wrap(err, "failed to get existing deployment")
		}
		imagePullSecrets = existingDeployment.Spec.Template.Spec.ImagePullSecrets
		containers = existingDeployment.Spec.Template.Spec.Containers
	} else {
		existingStatefulSet, err := clientset.AppsV1().StatefulSets(namespace).Get(context.TODO(), "kotsadm", metav1.GetOptions{})
		if err != nil {
			return imageName, pullSecret, errors.Wrap(err, "failed to get existing statefulset")
		}
		imagePullSecrets = existingStatefulSet.Spec.Template.Spec.ImagePullSecrets
		containers = existingStatefulSet.Spec.Template.Spec.Containers
	}

	for _, container := range containers {
		if container.Name == "kotsadm" {
			imageName = container.Image
			break
		}
	}
	if imageName == "" {
		return imageName, pullSecret, errors.New("container not found")
	}

	if len(imagePullSecrets) > 0 {
		existingSecret := imagePullSecrets[0]
		pullSecret = &troubleshootv1beta2.ImagePullSecrets{
			Name: existingSecret.Name,
		}
	}

	return imageName, pullSecret, nil
}

func populateImages(supportBundle *troubleshootv1beta2.SupportBundle, imageName string, pullSecret *troubleshootv1beta2.ImagePullSecrets) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()

	collects := []*troubleshootv1beta2.Collect{}
	for _, collect := range next.Spec.Collectors {
		if collect.Collectd != nil && collect.Collectd.Image == "alpine" { // TODO: is this too strong of an assumption?
			collect.Collectd.Image = imageName
			collect.Collectd.ImagePullSecret = pullSecret
		}
		if collect.CopyFromHost != nil && collect.CopyFromHost.Image == "alpine" { // TODO: is this too strong of an assumption?
			collect.CopyFromHost.Image = imageName
			collect.CopyFromHost.ImagePullSecret = pullSecret
		}
		collects = append(collects, collect)
	}
	next.Spec.Collectors = collects

	return next
}

// removeKurlCollectors removes collectors from the default support bundle spec that are specific to kURL clusters
func removeKurlCollectors(supportBundle *troubleshootv1beta2.SupportBundle) *troubleshootv1beta2.SupportBundle {
	next := supportBundle.DeepCopy()

	collects := []*troubleshootv1beta2.Collect{}
	for _, collect := range next.Spec.Collectors {
		if collect.Ceph != nil {
			continue
		}
		if collect.Longhorn != nil {
			continue
		}
		if collect.Collectd != nil {
			continue
		}
		if collect.Exec != nil {
			if collect.Exec.Name == "kots/kurl/weave" {
				continue
			}
		}
		if collect.Logs != nil {
			if collect.Logs.Name == "kots/kurl/weave" {
				continue
			}
			if collect.Logs.Namespace == "kurl" || collect.Logs.Namespace == "rook-ceph" {
				continue
			}
		}
		if collect.ConfigMap != nil && collect.ConfigMap.Namespace == "kurl" {
			continue
		}
		if collect.CopyFromHost != nil && collect.CopyFromHost.CollectorName == "kurl-host-preflights" {
			continue
		}
		collects = append(collects, collect)
	}
	next.Spec.Collectors = collects

	return next
}

// removeKurlAnalyzers removes analyzers from the default support bundle spec that are specific to kURL clusters
func removeKurlAnalyzers(analyzers []*troubleshootv1beta2.Analyze) []*troubleshootv1beta2.Analyze {

	analyze := []*troubleshootv1beta2.Analyze{}

	for _, analyzer := range analyzers {
		if analyzer.CephStatus != nil {
			continue
		}
		if analyzer.Longhorn != nil {
			continue
		}
		if analyzer.WeaveReport != nil {
			continue
		}
		if analyzer.TextAnalyze != nil {
			checkName := analyzer.TextAnalyze.CheckName

			if checkName == "Weave Report" || checkName == "Weave Status" {
				continue
			}
			if checkName == "Flannel: can read net-conf.json" || checkName == "Flannel: has access" {
				continue
			}
		}
		analyze = append(analyze, analyzer)
	}

	return analyze
}
