// Command feedback runs the feedback.miloapis.com aggregated API server and
// its supporting tasks.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	apiopenapi "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/cli"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"

	feedbackapiserver "go.miloapis.com/feedback/internal/apiserver"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/postgres"
	"go.miloapis.com/feedback/internal/version"
	"go.miloapis.com/feedback/pkg/generated/openapi"

	// Register the JSON logging format.
	_ "k8s.io/component-base/logs/json/register"
)

// postgresDSNEnv is read when --postgres-dsn is not set, so the DSN (which
// carries a password) can come from a Secret instead of the command line.
const postgresDSNEnv = "FEEDBACK_POSTGRES_DSN"

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilruntime.Must(utilfeature.DefaultMutableFeatureGate.Set("LoggingBetaOptions=true"))
	utilruntime.Must(utilfeature.DefaultMutableFeatureGate.Set("RemoteRequestHeaderUID=true"))
}

func main() {
	os.Exit(cli.Run(newRootCommand()))
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Feedback - roadmap, votes and submissions for Milo",
		Long: `Feedback serves the feedback.miloapis.com API: public roadmap posts synced
from GitHub, votes, submissions and security reports, stored in Postgres.`,
	}
	cmd.AddCommand(newServeCommand(), newMigrateCommand(), newVersionCommand())
	return cmd
}

func newServeCommand() *cobra.Command {
	o := newServerOptions()
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.Complete(); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			return run(cmd.Context(), o)
		},
	}
	o.AddFlags(cmd.Flags())
	logsapi.AddFlags(o.Logs, cmd.Flags())
	return cmd
}

func newMigrateCommand() *cobra.Command {
	var cfg postgres.Config
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending Postgres schema migrations",
		Long: `Apply every pending schema migration and exit. Migrations are forward-only
and run as a Job before the apiserver rolls out.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cfg.DSN == "" {
				cfg.DSN = os.Getenv(postgresDSNEnv)
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			return postgres.Migrate(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.DSN, "postgres-dsn", "", "Postgres connection string. Defaults to $"+postgresDSNEnv+".")
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(*cobra.Command, []string) {
			i := version.Get()
			fmt.Printf("Feedback Server\n")
			fmt.Printf("  Version:       %s\n", i.Version)
			fmt.Printf("  Git Commit:    %s\n", i.GitCommit)
			fmt.Printf("  Git Tree:      %s\n", i.GitTreeState)
			fmt.Printf("  Build Date:    %s\n", i.BuildDate)
			fmt.Printf("  Go Version:    %s\n", i.GoVersion)
			fmt.Printf("  Platform:      %s\n", i.Platform)
		},
	}
}

type serverOptions struct {
	RecommendedOptions *options.RecommendedOptions
	Logs               *logsapi.LoggingConfiguration
	Postgres           postgres.Config
	Identities         identity.Identities
	VoteRetention      time.Duration
}

func newServerOptions() *serverOptions {
	o := &serverOptions{
		RecommendedOptions: options.NewRecommendedOptions(
			"/registry/feedback.miloapis.com",
			feedbackapiserver.Codecs.LegacyCodec(feedbackapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs:          logsapi.NewLoggingConfiguration(),
		Postgres:      postgres.Config{MaxConns: 20, ConnectTimeout: 10 * time.Second},
		Identities:    identity.Default,
		VoteRetention: 30 * 24 * time.Hour,
	}
	// No admission plugins: validation lives in the registry strategies.
	o.RecommendedOptions.Admission = nil
	return o
}

func (o *serverOptions) AddFlags(fs *pflag.FlagSet) {
	o.RecommendedOptions.AddFlags(fs)
	fs.StringVar(&o.Postgres.DSN, "postgres-dsn", o.Postgres.DSN,
		"Postgres connection string. Defaults to $"+postgresDSNEnv+".")
	fs.Int32Var(&o.Postgres.MaxConns, "postgres-max-conns", o.Postgres.MaxConns,
		"Maximum number of connections in the Postgres pool.")
	fs.DurationVar(&o.Postgres.ConnectTimeout, "postgres-connect-timeout", o.Postgres.ConnectTimeout,
		"Timeout for the initial Postgres connection.")
	fs.StringSliceVar(&o.Identities.Sync, "sync-users", o.Identities.Sync,
		"Usernames of feedback-sync, the only writer of posts, releases and changelog entries.")
	fs.StringSliceVar(&o.Identities.Controller, "controller-users", o.Identities.Controller,
		"Usernames of feedback-controller, which owns delivery tracking on posts.")
	fs.DurationVar(&o.VoteRetention, "vote-retention", o.VoteRetention,
		"How long votes on a deleted post are kept in case it returns. 0 disables the purge.")
}

func (o *serverOptions) Complete() error {
	if o.Postgres.DSN == "" {
		o.Postgres.DSN = os.Getenv(postgresDSNEnv)
	}
	return nil
}

func (o *serverOptions) Validate() error {
	var errs []error
	errs = append(errs, o.RecommendedOptions.Validate()...)
	if err := o.Postgres.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("--postgres-dsn: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid options: %v", errs)
	}
	return nil
}

func (o *serverOptions) Config() (*feedbackapiserver.Config, error) {
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, nil); err != nil {
		return nil, fmt.Errorf("create self-signed certificates: %w", err)
	}

	genericConfig := genericapiserver.NewRecommendedConfig(feedbackapiserver.Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.35", "", "")

	// OpenAPI definitions are keyed by Go package path, but server-side apply
	// looks them up by REST-friendly name. Translate before asking the namer so
	// GVK extensions are found, and build $refs with the same names. This is
	// the same approach as milo-os/activity and milo-os/search.
	namer := apiopenapi.NewDefinitionNamer(feedbackapiserver.Scheme)
	getDefinitionName := func(name string) (string, spec.Extensions) {
		if strings.Contains(name, "/") {
			name = openapiutil.ToRESTFriendlyName(name)
		}
		return namer.GetDefinitionName(name)
	}
	refFor := func(prefix string) openapicommon.ReferenceCallback {
		return func(name string) spec.Ref {
			defName, _ := getDefinitionName(name)
			return spec.MustCreateRef(prefix + openapicommon.EscapeJsonPointer(defName))
		}
	}

	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(openapi.GetOpenAPIDefinitions, namer)
	genericConfig.OpenAPIV3Config.Info.Title = "Feedback"
	genericConfig.OpenAPIV3Config.Info.Version = version.Version
	genericConfig.OpenAPIV3Config.GetDefinitionName = getDefinitionName
	genericConfig.OpenAPIV3Config.Definitions = openapi.GetOpenAPIDefinitions(refFor("#/components/schemas/"))

	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(openapi.GetOpenAPIDefinitions, namer)
	genericConfig.OpenAPIConfig.Info.Title = "Feedback"
	genericConfig.OpenAPIConfig.Info.Version = version.Version
	genericConfig.OpenAPIConfig.GetDefinitionName = getDefinitionName
	genericConfig.OpenAPIConfig.Definitions = openapi.GetOpenAPIDefinitions(refFor("#/definitions/"))

	if err := o.RecommendedOptions.ApplyTo(genericConfig); err != nil {
		return nil, fmt.Errorf("apply recommended options: %w", err)
	}

	return &feedbackapiserver.Config{
		GenericConfig: genericConfig,
		ExtraConfig: feedbackapiserver.ExtraConfig{
			Postgres:      o.Postgres,
			Identities:    o.Identities,
			VoteRetention: o.VoteRetention,
		},
	}, nil
}

func run(ctx context.Context, o *serverOptions) error {
	if err := logsapi.ValidateAndApply(o.Logs, utilfeature.DefaultMutableFeatureGate); err != nil {
		return fmt.Errorf("apply logging configuration: %w", err)
	}
	defer logs.FlushLogs()

	cfg, err := o.Config()
	if err != nil {
		return err
	}
	server, err := cfg.Complete().New(ctx)
	if err != nil {
		return err
	}
	klog.Info("Starting Feedback server")
	return server.Run(ctx)
}
