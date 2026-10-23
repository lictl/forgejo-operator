/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	forgejov1alpha1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/autopilot"
	"github.com/lictl/forgejo-operator/internal/controller"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(forgejov1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var watchNamespaces string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&forgejo.ClusterDomain, "cluster-domain", forgejo.ClusterDomain,
		"DNS domain of the cluster used for in-cluster Service names (default: CLUSTER_DOMAIN, the pod's resolv.conf, or cluster.local).")
	flag.StringVar(&watchNamespaces, "watch-namespace", os.Getenv("WATCH_NAMESPACE"),
		"Comma-separated namespaces to watch. Empty watches all namespaces.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Events rejected because their namespace is being deleted are logged at V(1), not as
	// errors (see forgejo.QuietTerminatingNamespaces).
	ctrl.SetLogger(forgejo.QuietTerminatingNamespaces(zap.New(zap.UseFlagOptions(&opts))))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		Cache:                  cacheOptions(watchNamespaces),
		NewClient:              newClient,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "8780ea83.lucas.icu",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	autopilot.SetRESTConfig(mgr.GetConfig())

	if err := (&controller.ForgejoReconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorder("forgejo-controller"),
		WatchNamespaces: splitNamespaces(watchNamespaces),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "forgejo")
		os.Exit(1)
	}
	if err := (&controller.ForgejoBackupReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("forgejobackup-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "forgejobackup")
		os.Exit(1)
	}
	if err := (&controller.ForgejoRestoreReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("forgejorestore-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "forgejorestore")
		os.Exit(1)
	}
	if err := (&controller.ForgejoRunnerReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("forgejorunner-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "forgejorunner")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	reportUndecodableForgejos(ctx, mgr.GetAPIReader(), watchNamespaces)

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// reportUndecodableForgejos names every stored Forgejo that the typed informer cannot decode
// (e.g. a duration such as "1d" stored before the CRD validated durations). One such object
// fails the whole list, so the cache never syncs and the manager exits; the informer's own
// error does not say which object is at fault.
func reportUndecodableForgejos(ctx context.Context, r client.Reader, namespaces string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	nss := splitNamespaces(namespaces)
	if len(nss) == 0 {
		nss = []string{""} // all namespaces
	}
	for _, ns := range nss {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(forgejov1alpha1.GroupVersion.WithKind("ForgejoList"))
		if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
			setupLog.Info("Could not pre-check stored Forgejo objects", "namespace", ns, "error", err.Error())
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			raw, err := item.MarshalJSON()
			if err == nil {
				err = json.Unmarshal(raw, &forgejov1alpha1.Forgejo{})
			}
			if err != nil {
				setupLog.Error(err, "Stored Forgejo cannot be decoded; no Forgejo is reconciled until it is fixed or deleted",
					"namespace", item.GetNamespace(), "name", item.GetName())
			}
		}
	}
}

// cacheOptions restricts the manager cache to the given comma-separated namespaces and,
// for high-volume kinds, to objects carrying the operator's labels (forgejo.CacheScopes).
// managedFields are dropped from every cached object; nothing reads them.
func cacheOptions(namespaces string) cache.Options {
	opts := cache.Options{
		ByObject:         forgejo.CacheByObject(forgejo.CacheScopes()),
		DefaultTransform: cache.TransformStripManagedFields(),
	}
	for _, ns := range splitNamespaces(namespaces) {
		if opts.DefaultNamespaces == nil {
			opts.DefaultNamespaces = map[string]cache.Config{}
		}
		opts.DefaultNamespaces[ns] = cache.Config{}
	}
	return opts
}

// newClient builds the manager's client with a reader that keeps the label-restricted
// cache transparent to callers: reads the cache cannot answer (an unlabelled Secret or Pod,
// a List whose selector is broader than the cache's) go to the API server instead of
// returning NotFound or a partial list.
func newClient(config *rest.Config, options client.Options) (client.Client, error) {
	live, err := client.New(config, client.Options{
		HTTPClient: options.HTTPClient,
		Scheme:     options.Scheme,
		Mapper:     options.Mapper,
	})
	if err != nil {
		return nil, err
	}
	if options.Cache != nil && options.Cache.Reader != nil {
		reader, err := forgejo.NewScopedReader(options.Cache.Reader, live, options.Scheme, forgejo.CacheScopes())
		if err != nil {
			return nil, err
		}
		cacheOpts := *options.Cache
		cacheOpts.Reader = reader
		options.Cache = &cacheOpts
	}
	return client.New(config, options)
}

func splitNamespaces(namespaces string) []string {
	var out []string
	for ns := range strings.SplitSeq(namespaces, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
