// Command manager roda o controller do PreviewEnvironment.
package main

import (
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	previewv1alpha1 "github.com/RIGOandre/preview-operator/api/v1alpha1"
	"github.com/RIGOandre/preview-operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := previewv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		leaderElect    bool
		baseDomain     string
		ingressClass   string
		quotaCPU       string
		quotaMemory    string
		quotaPods      int64
		defaultCPU     string
		defaultMemory  string
		maxConcurrency int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "endereço do endpoint de métricas")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "endereço das probes")
	flag.BoolVar(&leaderElect, "leader-elect", false, "elege líder entre as réplicas do manager")
	flag.StringVar(&baseDomain, "base-domain", "", "domínio dos previews; vazio desliga o Ingress")
	flag.StringVar(&ingressClass, "ingress-class", "", "ingressClassName aplicada aos Ingress criados")
	flag.StringVar(&quotaCPU, "quota-cpu", "2", "teto de CPU do namespace de cada preview")
	flag.StringVar(&quotaMemory, "quota-memory", "2Gi", "teto de memória do namespace de cada preview")
	flag.Int64Var(&quotaPods, "quota-pods", 10, "teto de pods do namespace de cada preview")
	flag.StringVar(&defaultCPU, "default-cpu", "500m", "limite de CPU do container quando o spec não pede")
	flag.StringVar(&defaultMemory, "default-memory", "512Mi", "limite de memória do container quando o spec não pede")
	flag.IntVar(&maxConcurrency, "max-concurrent-reconciles", 4, "reconciles simultâneos")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	cfg, err := buildConfig(baseDomain, ingressClass, quotaCPU, quotaMemory, quotaPods, defaultCPU, defaultMemory)
	if err != nil {
		log.Error(err, "configuração inválida")
		os.Exit(1)
	}
	if baseDomain == "" {
		log.Info("sem --base-domain: os ambientes vão subir sem Ingress")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "preview-operator.rigo.dev",
	})
	if err != nil {
		log.Error(err, "não foi possível iniciar o manager")
		os.Exit(1)
	}

	reconciler := &controller.PreviewEnvironmentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("preview-operator"),
		Config:   cfg,
	}
	if err := reconciler.SetupWithManager(mgr, maxConcurrency); err != nil {
		log.Error(err, "não foi possível registrar o controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "não foi possível registrar o healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "não foi possível registrar o readyz")
		os.Exit(1)
	}

	log.Info("manager no ar", "baseDomain", baseDomain, "ingressClass", ingressClass)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "o manager parou com erro")
		os.Exit(1)
	}
}

// buildConfig valida as flags de quantidade antes de o manager subir. Um
// "2Gii" digitado errado tem que derrubar o processo no boot, não virar
// ResourceQuota inválida no primeiro pull request que aparecer.
func buildConfig(baseDomain, ingressClass, quotaCPU, quotaMemory string, quotaPods int64, defaultCPU, defaultMemory string) (controller.Config, error) {
	parse := func(flagName, raw string) (resource.Quantity, error) {
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			return q, fmt.Errorf("--%s=%q: %w", flagName, raw, err)
		}
		return q, nil
	}

	qCPU, err := parse("quota-cpu", quotaCPU)
	if err != nil {
		return controller.Config{}, err
	}
	qMem, err := parse("quota-memory", quotaMemory)
	if err != nil {
		return controller.Config{}, err
	}
	dCPU, err := parse("default-cpu", defaultCPU)
	if err != nil {
		return controller.Config{}, err
	}
	dMem, err := parse("default-memory", defaultMemory)
	if err != nil {
		return controller.Config{}, err
	}
	if quotaPods < 1 {
		return controller.Config{}, fmt.Errorf("--quota-pods=%d: precisa ser ao menos 1", quotaPods)
	}

	return controller.Config{
		BaseDomain:       baseDomain,
		IngressClassName: ingressClass,
		QuotaCPU:         qCPU,
		QuotaMemory:      qMem,
		QuotaPods:        quotaPods,
		DefaultResources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceCPU: dCPU, corev1.ResourceMemory: dMem},
			Requests: corev1.ResourceList{corev1.ResourceCPU: dCPU, corev1.ResourceMemory: dMem},
		},
	}, nil
}
