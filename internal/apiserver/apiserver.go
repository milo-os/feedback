// Package apiserver wires the feedback.miloapis.com aggregated API server.
package apiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/postgres"
	"go.miloapis.com/feedback/internal/registry/feedbacksource"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/registry/release"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var (
	// Scheme is the runtime type registry for the server.
	Scheme = runtime.NewScheme()
	// Codecs serializes objects in Scheme.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	install.Install(Scheme)

	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})

	// There is no separate internal version: register the v1alpha1 types as
	// internal too so the generic registry can convert and watch them.
	Scheme.AddKnownTypes(schema.GroupVersion{Group: feedbackv1alpha1.GroupName, Version: runtime.APIVersionInternal},
		feedbackv1alpha1.KnownTypes()...)

	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// ExtraConfig holds feedback-specific configuration.
type ExtraConfig struct {
	Postgres postgres.Config
	// Identities are the usernames of feedback-sync and feedback-controller,
	// which own particular fields.
	Identities identity.Identities
	// VoteRetention is how long votes on a deleted post are kept in case the
	// post comes back.
	VoteRetention time.Duration
}

// Config combines the generic and feedback-specific configuration.
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig can only be built by Complete.
type CompletedConfig struct {
	*completedConfig
}

// Complete fills in defaults.
func (cfg *Config) Complete() CompletedConfig {
	return CompletedConfig{&completedConfig{cfg.GenericConfig.Complete(), &cfg.ExtraConfig}}
}

// FeedbackServer is the feedback aggregated API server.
type FeedbackServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
	pool             *pgxpool.Pool
}

// New connects to Postgres and installs the feedback API group.
func (c completedConfig) New(ctx context.Context) (*FeedbackServer, error) {
	pool, err := postgres.NewPool(ctx, c.ExtraConfig.Postgres)
	if err != nil {
		return nil, err
	}

	genericServer, err := c.GenericConfig.New("feedback-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := genericServer.AddReadyzChecks(postgres.HealthCheck{Pool: pool}); err != nil {
		pool.Close()
		return nil, fmt.Errorf("add postgres readyz check: %w", err)
	}

	s := &FeedbackServer{GenericAPIServer: genericServer, pool: pool}

	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(feedbackv1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	v1alpha1Storage := map[string]rest.Storage{}

	// FeedbackSource is the one resource stored in etcd.
	sourceStorage, err := feedbacksource.NewREST(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create FeedbackSource storage: %w", err)
	}
	v1alpha1Storage["feedbacksources"] = sourceStorage.Store
	v1alpha1Storage["feedbacksources/status"] = sourceStorage.Status

	// Everything else is in Postgres.
	db := &pg.DB{Pool: pool}
	ids := c.ExtraConfig.Identities

	posts, err := post.NewREST(db, Scheme, ids)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create Post storage: %w", err)
	}
	v1alpha1Storage["posts"] = posts.Store
	v1alpha1Storage["posts/status"] = posts.Status

	releases, err := release.NewReleaseREST(db, Scheme, ids)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create Release storage: %w", err)
	}
	v1alpha1Storage["releases"] = releases.Store
	v1alpha1Storage["releases/status"] = releases.Status

	changelog, err := release.NewChangelogREST(db, Scheme, ids)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create ChangelogEntry storage: %w", err)
	}
	v1alpha1Storage["changelogentries"] = changelog.Store
	v1alpha1Storage["changelogentries/status"] = changelog.Status

	// Other Postgres-backed resources (PostQuery, Vote, Submission, ...) are
	// added here as they are implemented. See datum-cloud/enhancements#943.

	apiGroupInfo.VersionedResourcesStorageMap[feedbackv1alpha1.SchemeGroupVersion.Version] = v1alpha1Storage
	if err := genericServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		pool.Close()
		return nil, err
	}

	retention := c.ExtraConfig.VoteRetention
	genericServer.AddPostStartHookOrDie("feedback-vote-retention", func(hc genericapiserver.PostStartHookContext) error {
		go runVoteRetention(hc.Context, db, retention)
		return nil
	})

	klog.Info("Feedback server initialized")
	return s, nil
}

// runVoteRetention purges votes on long-deleted posts every hour.
func runVoteRetention(ctx context.Context, db *pg.DB, retention time.Duration) {
	if retention <= 0 {
		klog.Info("Vote retention purge disabled")
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := post.PurgeExpiredVotes(ctx, db, retention)
		switch {
		case err != nil && ctx.Err() == nil:
			klog.ErrorS(err, "Purging votes on deleted posts failed")
		case n > 0:
			klog.InfoS("Purged votes on deleted posts", "votes", n, "retention", retention)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run serves until ctx is cancelled, then closes the Postgres pool.
func (s *FeedbackServer) Run(ctx context.Context) error {
	defer s.pool.Close()
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
