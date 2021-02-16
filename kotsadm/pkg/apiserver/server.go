package apiserver

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/replicatedhq/kots/kotsadm/pkg/automation"
	"github.com/replicatedhq/kots/kotsadm/pkg/handlers"
	"github.com/replicatedhq/kots/kotsadm/pkg/informers"
	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/policy"
	"github.com/replicatedhq/kots/kotsadm/pkg/snapshotscheduler"
	"github.com/replicatedhq/kots/kotsadm/pkg/socketservice"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
	"github.com/replicatedhq/kots/kotsadm/pkg/supportbundle"
	"github.com/replicatedhq/kots/kotsadm/pkg/updatechecker"
	"github.com/replicatedhq/kots/pkg/rbac"
	"github.com/segmentio/ksuid"
	corev1 "k8s.io/api/core/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

func Start() {
	log.Printf("kotsadm version %s\n", os.Getenv("VERSION"))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	if err := store.GetStore().WaitForReady(ctx); err != nil {
		panic(err)
	}
	cancel()

	if err := bootstrap(); err != nil {
		panic(err)
	}

	store.GetStore().RunMigrations()

	err := bootstrapIdentity()
	if err != nil {
		panic(err)
	}

	detectInstanceID()

	supportbundle.StartServer()

	if err := informers.Start(); err != nil {
		log.Println("Failed to start informers", err)
	}

	if err := updatechecker.Start(); err != nil {
		log.Println("Failed to start update checker", err)
	}

	if err := snapshotscheduler.Start(); err != nil {
		log.Println("Failed to start snapshot scheduler", err)
	}

	waitForAirgap, err := automation.NeedToWaitForAirgapApp()
	if err != nil {
		log.Println("Failed to check if airgap install is in progress", err)
	} else if !waitForAirgap {
		if err := automation.AutomateInstall(); err != nil {
			log.Println("Failed to run automated installs", err)
		}
	}

	r := mux.NewRouter()

	r.Use(handlers.CorsMiddleware)
	r.Methods("OPTIONS").HandlerFunc(handlers.CORS)

	handler := &handlers.Handler{}

	/**********************************************************************
	* Unauthenticated routes
	**********************************************************************/

	r.HandleFunc("/healthz", handler.Healthz)
	r.HandleFunc("/api/v1/login", handler.Login)
	r.HandleFunc("/api/v1/login/info", handler.GetLoginInfo)
	r.HandleFunc("/api/v1/logout", handler.Logout) // this route uses its own auth
	r.Path("/api/v1/metadata").Methods("GET").HandlerFunc(handler.Metadata)

	r.HandleFunc("/api/v1/oidc/login", handler.OIDCLogin)
	r.HandleFunc("/api/v1/oidc/login/callback", handler.OIDCLoginCallback)

	r.Path("/api/v1/troubleshoot/{appId}/{bundleId}").Methods("PUT").HandlerFunc(handler.UploadSupportBundle)
	r.Path("/api/v1/troubleshoot/supportbundle/{bundleId}/redactions").Methods("PUT").HandlerFunc(handler.SetSupportBundleRedactions)
	r.Path("/api/v1/preflight/app/{appSlug}/sequence/{sequence}").Methods("POST").HandlerFunc(handler.PostPreflightStatus)

	// This the handler for license API and should be called by the application only.
	r.Path("/license/v1/license").Methods("GET").HandlerFunc(handler.GetPlatformLicenseCompatibility)

	/**********************************************************************
	* Cluster auth routes (functions that the operator calls)
	**********************************************************************/

	r.Path("/api/v1/appstatus").Methods("PUT").HandlerFunc(handler.SetAppStatus)
	r.Path("/api/v1/deploy/result").Methods("PUT").HandlerFunc(handler.UpdateDeployResult)
	r.Path("/api/v1/undeploy/result").Methods("PUT").HandlerFunc(handler.UpdateUndeployResult)
	r.Handle("/socket.io/", socketservice.Start())

	/**********************************************************************
	* KOTS token auth routes
	**********************************************************************/

	r.Path("/api/v1/kots/ports").Methods("GET").HandlerFunc(handler.GetApplicationPorts)
	r.Path("/api/v1/upload").Methods("PUT").HandlerFunc(handler.UploadExistingApp)
	r.Path("/api/v1/download").Methods("GET").HandlerFunc(handler.DownloadApp)
	r.Path("/api/v1/airgap/install").Methods("POST").HandlerFunc(handler.UploadInitialAirgapApp)

	/**********************************************************************
	* Session auth routes
	**********************************************************************/

	kotsStore := store.GetStore()
	policyMiddleware := policy.NewMiddleware(kotsStore, rbac.DefaultRoles())

	sessionAuthQuietRouter := r.PathPrefix("").Subrouter()
	sessionAuthQuietRouter.Use(handlers.RequireValidSessionQuietMiddleware(kotsStore))

	sessionAuthQuietRouter.Path("/api/v1/ping").Methods("GET").HandlerFunc(handler.Ping)

	handlers.RegisterSessionAuthRoutes(r.PathPrefix("").Subrouter(), kotsStore, handler, policyMiddleware)

	// Prevent API requests that don't match anything in this router from returning UI content
	r.PathPrefix("/api").Handler(handlers.StatusNotFoundHandler{})

	/**********************************************************************
	* Static routes
	**********************************************************************/

	// to avoid confusion, we don't serve this in the dev env...
	if os.Getenv("DISABLE_SPA_SERVING") != "1" {
		spa := handlers.SPAHandler{StaticPath: filepath.Join("web", "dist"), IndexPath: "index.html"}
		r.PathPrefix("/").Handler(spa)
	} else if os.Getenv("ENABLE_WEB_PROXY") == "1" { // for dev env
		u, err := url.Parse("http://kotsadm-web:30000")
		if err != nil {
			panic(err)
		}
		upstream := httputil.NewSingleHostReverseProxy(u)
		webProxy := func(upstream *httputil.ReverseProxy) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, r *http.Request) {
				r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
				upstream.ServeHTTP(w, r)
			}
		}(upstream)
		r.PathPrefix("/").HandlerFunc(webProxy)
	}

	srv := &http.Server{
		Handler: r,
		Addr:    ":3000",
	}

	fmt.Printf("Starting kotsadm API on port %d...\n", 3000)

	log.Fatal(srv.ListenAndServe())
}

/*
 * Detects the InstanceID of kodsadm pod across restores
 */
func detectInstanceID() {

	debug.PrintStack()
	fmt.Println("Detecting Instance")

	// Retrieve the ClusterID from store
	clusters, err := store.GetStore().ListClusters()
	if err != nil {
		logger.Errorf("Failed to list clusters %v", err)
		return
	}
	if len(clusters) == 0 {
		logger.Error(errors.New("No clusters found"))
		return
	}
	clusterID := clusters[0].ClusterID
	// ID to get into configmap
	fmt.Println(clusters[0].ClusterID)

	// Write a Query to set/get an Event from the Store
	keyExists, err := store.GetStore().GetKotsAdmEventStatus()
	// failed to scan
	if err != nil {
		logger.Errorf("Failed to scan %v", err)
		return
	}

	// if the key does not exist, likely a Restore scenario
	var (
		configMapName = "kotsadm-id"
	)
	if keyExists == true {
		fmt.Println("Key exists")
		exists, err := isConfigMapPresent(configMapName)
		if err != nil {
			logger.Errorf("Config map check error %v", err)
			return

		}
		if exists == true {
			// do nothing
			return
		}
		//generate guid and use that as clusterId to identify that as a different install
		clusterID = ksuid.New().String()
		_, err = createConfigMap(configMapName, clusterID)
		if err != nil {
			logger.Errorf("Failed to to create config map %v", err)
			return
		}
	}

	// if the key exists, likely a fresh Install
	if keyExists == false {
		fmt.Println("-------- Key does not exist --------")
		_, err := createConfigMap(configMapName, clusterID)
		if err != nil {
			logger.Errorf("Failed to scan %v", err)
			return
		}
		err = store.GetStore().SetKotsAdmEventStatus()
		if err != nil {
			logger.Errorf("Failed to scan %v", err)
			return
		}
	}

}

func isConfigMapPresent(configMapName string) (bool, error) {

	cfg, err := k8sconfig.GetConfig()
	if err != nil {
		return false, errors.Wrap(err, "failed to get kubernetes config")
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return false, errors.Wrap(err, "failed to get clientset")
	}
	namespace := os.Getenv("POD_NAMESPACE")
	existingConfigmap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil && !kuberneteserrors.IsNotFound(err) {
		return false, errors.Wrap(err, "failed to get configmap")
	} else if kuberneteserrors.IsNotFound(err) {
		return false, nil
	}
	if existingConfigmap != nil {
		fmt.Println("-------------Existing config map ------", existingConfigmap.Data["id"], "REFLECT:", reflect.TypeOf(existingConfigmap))
		return true, nil
	}
	return false, nil

}

func createConfigMap(configMapName string, clusterID string) (*corev1.ConfigMap, error) {

	cfg, err := k8sconfig.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get kubernetes config")
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get clientset")
	}
	configmap := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: os.Getenv("POD_NAMESPACE"),
			Labels: map[string]string{
				"kots.io/kotsadm": "true",
			},
		},
		Data: map[string]string{"id": clusterID},
	}

	createdConfigmap, err := clientset.CoreV1().ConfigMaps(os.Getenv("POD_NAMESPACE")).Create(context.TODO(), &configmap, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create configmap")
	}
	fmt.Println("------- CreatedConfig map----", createdConfigmap)

	return createdConfigmap, nil

}
