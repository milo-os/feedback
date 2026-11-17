package main

import (
	"flag"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.miloapis.com/feedback/internal/version"
	"go.miloapis.com/feedback/internal/webhook"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(feedbackv1alpha1.AddToScheme(scheme))
}

type operatorOptions struct {
	miloKubeconfig          string
	metricsAddr             string
	probeAddr               string
	leaderElect             bool
	leaderElectionNamespace string
	webhookPort             int
	webhookCertDir          string
}

func newOperatorCommand() *cobra.Command {
	o := operatorOptions{}
	zapOpts := zap.Options{}
	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Reconcile feedback resources in Milo and serve their admission webhooks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
			return runOperator(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.miloKubeconfig, "milo-kubeconfig", "", "Kubeconfig for Milo, where the feedback resources live. Empty uses the in-cluster config.")
	f.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to; 0 disables it.")
	f.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "Address the health probes bind to.")
	f.BoolVar(&o.leaderElect, "leader-elect", false, "Elect a leader, so only one replica reconciles. The lease lives in the cluster the operator runs in.")
	f.StringVar(&o.leaderElectionNamespace, "leader-election-namespace", "feedback-system", "Namespace of the leader election lease.")
	f.IntVar(&o.webhookPort, "webhook-port", 9443, "Port the admission webhook server listens on.")
	f.StringVar(&o.webhookCertDir, "webhook-cert-dir", "/etc/kubernetes/pki/webhook", "Directory holding tls.crt and tls.key for the webhook server.")
	goFlags := flag.NewFlagSet("zap", flag.ExitOnError)
	zapOpts.BindFlags(goFlags)
	f.AddGoFlagSet(goFlags)
	return cmd
}

func runOperator(o operatorOptions) error {
	log := ctrl.Log.WithName("setup")
	i := version.Get()
	log.Info("Starting feedback operator", "version", i.Version, "gitCommit", i.GitCommit)

	miloConfig, err := restConfig(o.miloKubeconfig)
	if err != nil {
		return fmt.Errorf("loading Milo config: %w", err)
	}
	// The lease lives where the operator runs, not in Milo.
	localConfig, err := rest.InClusterConfig()
	if err != nil {
		localConfig = miloConfig
	}

	mgr, err := ctrl.NewManager(miloConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:        o.probeAddr,
		LeaderElection:                o.leaderElect,
		LeaderElectionID:              "feedback.miloapis.com",
		LeaderElectionNamespace:       o.leaderElectionNamespace,
		LeaderElectionConfig:          localConfig,
		LeaderElectionReleaseOnCancel: true,
		WebhookServer:                 ctrlwebhook.NewServer(ctrlwebhook.Options{Port: o.webhookPort, CertDir: o.webhookCertDir}),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	webhook.Register(mgr)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	log.Info("Starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}

// restConfig loads kubeconfig, or the in-cluster config when it is empty.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
