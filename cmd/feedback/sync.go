package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.miloapis.com/feedback/internal/github"
	"go.miloapis.com/feedback/internal/githubsync"
	"go.miloapis.com/feedback/internal/version"
)

type syncOptions struct {
	miloKubeconfig          string
	metricsAddr             string
	probeAddr               string
	leaderElect             bool
	leaderElectionNamespace string
	appID                   int64
	privateKeyFile          string
	githubURL               string
	interval                time.Duration
	retry                   time.Duration
	minRateLimit            int
}

func newSyncCommand() *cobra.Command {
	o := syncOptions{}
	zapOpts := zap.Options{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync posts from the GitHub issues each FeedbackSource selects",
		Long: `sync sweeps each FeedbackSource in Milo on an interval: it searches GitHub
for the issues the source includes, creates, updates and deletes their posts,
and reports the result on the source's status.

It authenticates as the GitHub App. The app ID comes from --github-app-id or
GITHUB_APP_ID, and the private key from --github-app-private-key-file or, if
that is empty, the PEM in GITHUB_APP_PRIVATE_KEY. Each FeedbackSource names
its installation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
			return runSync(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.miloKubeconfig, "milo-kubeconfig", "", "Kubeconfig for Milo, where the feedback resources live. Empty uses the in-cluster config.")
	f.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to; 0 disables it.")
	f.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "Address the health probes bind to.")
	f.BoolVar(&o.leaderElect, "leader-elect", false, "Elect a leader, so only one replica syncs. The lease lives in the cluster sync runs in.")
	f.StringVar(&o.leaderElectionNamespace, "leader-election-namespace", "feedback-system", "Namespace of the leader election lease.")
	f.Int64Var(&o.appID, "github-app-id", 0, "GitHub App ID. Defaults to GITHUB_APP_ID.")
	f.StringVar(&o.privateKeyFile, "github-app-private-key-file", "", "File holding the GitHub App's PEM private key. Empty reads the key from GITHUB_APP_PRIVATE_KEY.")
	f.StringVar(&o.githubURL, "github-api-url", github.DefaultBaseURL, "GitHub API base URL.")
	f.DurationVar(&o.interval, "sweep-interval", githubsync.DefaultInterval, "Time between sweeps of a FeedbackSource.")
	f.DurationVar(&o.retry, "sweep-retry", githubsync.DefaultRetry, "Time before a failed sweep is tried again.")
	f.IntVar(&o.minRateLimit, "min-rate-limit", githubsync.DefaultMinRateLimit, "GitHub API points a sweep keeps in hand; with fewer left, it waits for the limit to reset.")
	goFlags := flag.NewFlagSet("zap", flag.ExitOnError)
	zapOpts.BindFlags(goFlags)
	f.AddGoFlagSet(goFlags)
	return cmd
}

// appID is the --github-app-id flag, or GITHUB_APP_ID when it is unset.
// Secrets often end in a newline, so surrounding space is ignored.
func appID(flag int64) (int64, error) {
	if flag != 0 {
		return flag, nil
	}
	v := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	if v == "" {
		return 0, errors.New("no GitHub App ID: set --github-app-id or GITHUB_APP_ID")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	return id, nil
}

// privateKey reads the GitHub App's private key from the file, or from
// GITHUB_APP_PRIVATE_KEY when there is none.
func privateKey(file string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	if k := os.Getenv("GITHUB_APP_PRIVATE_KEY"); k != "" {
		return []byte(k), nil
	}
	return nil, errors.New("no GitHub App private key: set --github-app-private-key-file or GITHUB_APP_PRIVATE_KEY")
}

// leaseConfig is the config for the leader-election lease, which lives in
// the cluster feedback-sync runs in: it has no lease RBAC in Milo.
func leaseConfig(leaderElect bool, inCluster func() (*rest.Config, error)) (*rest.Config, error) {
	if !leaderElect {
		return nil, nil
	}
	cfg, err := inCluster()
	if err != nil {
		return nil, fmt.Errorf("--leader-elect needs an in-cluster config, for a lease in the cluster feedback-sync runs in: %w", err)
	}
	return cfg, nil
}

func runSync(o syncOptions) error {
	log := ctrl.Log.WithName("setup")
	i := version.Get()
	log.Info("Starting feedback-sync", "version", i.Version, "gitCommit", i.GitCommit)

	key, err := privateKey(o.privateKeyFile)
	if err != nil {
		return err
	}
	id, err := appID(o.appID)
	if err != nil {
		return err
	}
	app, err := github.NewApp(id, key, github.WithBaseURL(o.githubURL))
	if err != nil {
		return err
	}

	miloConfig, err := restConfig(o.miloKubeconfig)
	if err != nil {
		return fmt.Errorf("loading Milo config: %w", err)
	}
	leaseCfg, err := leaseConfig(o.leaderElect, rest.InClusterConfig)
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(miloConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:        o.probeAddr,
		LeaderElection:                o.leaderElect,
		LeaderElectionID:              "sync.feedback.miloapis.com",
		LeaderElectionNamespace:       o.leaderElectionNamespace,
		LeaderElectionConfig:          leaseCfg,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	syncer := &githubsync.Syncer{Client: mgr.GetClient(), GitHub: githubsync.App{App: app}}
	r := &githubsync.SourceReconciler{
		Client:       mgr.GetClient(),
		Syncer:       syncer,
		Interval:     o.interval,
		Retry:        o.retry,
		MinRateLimit: o.minRateLimit,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the sweep: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	log.Info("Starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}
