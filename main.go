package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	cliflag "k8s.io/component-base/cli/flag"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt-aie-webhook/pkg/config"
	webhookpkg "kubevirt.io/kubevirt-aie-webhook/pkg/webhook"
)

const (
	configMapName = "kubevirt-aie-launcher-config"
	configDataKey = "config.yaml"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kubevirtv1.AddToScheme(scheme))
}

// configMapReconciler watches the launcher config ConfigMap and updates the store.
type configMapReconciler struct {
	client client.Client
	store  *config.ConfigStore
	ns     string
}

func (r *configMapReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if req.Name != configMapName || req.Namespace != r.ns {
		return reconcile.Result{}, nil
	}

	var cm corev1.ConfigMap
	if err := r.client.Get(ctx, req.NamespacedName, &cm); err != nil {
		log.Error(err, "unable to fetch ConfigMap")
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	data, ok := cm.Data[configDataKey]
	if !ok {
		log.Info("ConfigMap missing data key", "key", configDataKey)
		return reconcile.Result{}, nil
	}

	if err := r.store.Update([]byte(data)); err != nil {
		log.Error(err, "failed to parse launcher config")
		return reconcile.Result{}, err
	}

	log.Info("launcher config updated")
	return reconcile.Result{}, nil
}

type serverConfig struct {
	metricsAddr   string
	probeAddr     string
	secureMetrics bool
	enableHTTP2   bool

	metricsCertPath string
	metricsCertName string
	metricsCertKey  string

	webhookCertPath string
	webhookCertName string
	webhookCertKey  string

	cipherSuites  string
	minTLSVersion string
}

func parseFlags() serverConfig {
	var cfg serverConfig

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)

	flag.StringVar(&cfg.cipherSuites, "tls-cipher-suites", "",
		"Comma-separated list of cipher suites for the server. "+
			"If omitted, the default Go cipher suites will be used. \n"+
			"Preferred values: "+strings.Join(cliflag.PreferredTLSCipherNames(), ", ")+". \n"+
			"Insecure values: "+strings.Join(cliflag.InsecureTLSCipherNames(), ", ")+".")
	flag.StringVar(&cfg.minTLSVersion, "tls-min-version", "",
		"Minimum TLS version supported. "+
			"Possible values: "+strings.Join(cliflag.TLSPossibleVersions(), ", "))
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	return cfg
}

func buildTLSOpts(cfg serverConfig) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	if !cfg.enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		})
	}

	tlsOpts = appendCipherSuites(setupLog, tlsOpts, cfg.cipherSuites)
	tlsOpts = appendMinTLSVersion(setupLog, tlsOpts, cfg.minTLSVersion)

	return tlsOpts
}

func newWebhookServer(cfg serverConfig, tlsOpts []func(*tls.Config)) webhook.Server {
	opts := webhook.Options{
		TLSOpts: tlsOpts,
	}

	if cfg.webhookCertPath != "" {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.webhookCertPath, "webhook-cert-name", cfg.webhookCertName, "webhook-cert-key", cfg.webhookCertKey)

		opts.CertDir = cfg.webhookCertPath
		opts.CertName = cfg.webhookCertName
		opts.KeyName = cfg.webhookCertKey
	}

	return webhook.NewServer(opts)
}

func newMetricsServerOptions(cfg serverConfig, tlsOpts []func(*tls.Config)) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if cfg.secureMetrics {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if cfg.metricsCertPath != "" {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName, "metrics-cert-key", cfg.metricsCertKey)

		opts.CertDir = cfg.metricsCertPath
		opts.CertName = cfg.metricsCertName
		opts.KeyName = cfg.metricsCertKey
	}

	return opts
}

func main() {
	cfg := parseFlags()

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "kubevirt"
	}

	tlsOpts := buildTLSOpts(cfg)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Field: fields.SelectorFromSet(fields.Set{
						"metadata.name": configMapName,
					}),
					Namespaces: map[string]cache.Config{
						namespace: {},
					},
				},
			},
		},
		Metrics:                newMetricsServerOptions(cfg, tlsOpts),
		HealthProbeBindAddress: cfg.probeAddr,
		WebhookServer:          newWebhookServer(cfg, tlsOpts),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	store := config.NewConfigStore()

	// Set up ConfigMap watcher
	cmReconciler := &configMapReconciler{
		client: mgr.GetClient(),
		store:  store,
		ns:     namespace,
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("configmap-watcher").
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() != configMapName || obj.GetNamespace() != namespace {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					},
				}}
			},
		)).
		Complete(cmReconciler); err != nil {
		setupLog.Error(err, "unable to create ConfigMap controller")
		os.Exit(1)
	}

	// Register mutating webhook handler using the manager's API reader for
	// VMI lookups. The manager's default client reads through the cache, which
	// only has an informer for ConfigMaps. With ReaderFailOnMissingInformer
	// enabled, any Get/List for a type without a running informer (such as
	// VirtualMachineInstance) will fail rather than lazily starting one.
	mgr.GetWebhookServer().Register("/mutate-pods", &webhook.Admission{
		Handler: &webhookpkg.VirtLauncherMutator{
			Client:  mgr.GetAPIReader(),
			Store:   store,
			Decoder: admission.NewDecoder(scheme),
		},
	})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func appendCipherSuites(setupLog logr.Logger, tlsOpts []func(*tls.Config), cipherSuitesFlag string) []func(*tls.Config) {
	if cipherSuitesFlag == "" {
		return tlsOpts
	}
	cipherSuites := strings.Split(cipherSuitesFlag, ",")
	cipherSuiteIDs, err := cliflag.TLSCipherSuites(cipherSuites)
	if err != nil {
		setupLog.Error(err, "failed to parse TLS cipher suites")
		os.Exit(1)
	}

	setCipherSuites := func(c *tls.Config) {
		setupLog.Info("setting tls cipher suites to " + strings.Join(cipherSuites, ", "))
		c.CipherSuites = cipherSuiteIDs
	}

	return append(tlsOpts, setCipherSuites)
}

func appendMinTLSVersion(setupLog logr.Logger, tlsOpts []func(*tls.Config), minTLSVersion string) []func(*tls.Config) {
	if minTLSVersion == "" {
		return tlsOpts
	}
	minTLSVersionID, err := cliflag.TLSVersion(minTLSVersion)
	if err != nil {
		setupLog.Error(err, "failed to parse TLS min version")
		os.Exit(1)
	}

	setMinTLSVersion := func(c *tls.Config) {
		setupLog.Info("setting tls min version to " + minTLSVersion)
		c.MinVersion = minTLSVersionID
	}

	return append(tlsOpts, setMinTLSVersion)
}
