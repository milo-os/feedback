package controller

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type fakeUsers struct{}

func (fakeUsers) Organizations(_ context.Context, user string) ([]string, error) {
	return []string{"org-of-" + user}, nil
}
func (fakeUsers) GitHubLogin(_ context.Context, user string) (string, error) {
	return "gh-" + user, nil
}

// clock is a settable time for the reconcilers.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// start runs the reconcilers against an envtest API server with the CRDs,
// and returns a client that reads straight from the API server.
func start(t *testing.T, now *clock) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run with task test")
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Tests start a manager per run; names repeat with -count.
		Controller: config.Controller{SkipNameValidation: ptr(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := Setup(ctx, mgr, Options{Users: fakeUsers{}, Now: now.Now}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// eventually retries check until it returns nil or time runs out.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type errorf string

func (e errorf) Error() string { return string(e) }

func newVote(post, user string) *v1.Vote {
	return &v1.Vote{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post, user)},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: user}, Source: v1.ClientWebsite},
	}
}

func TestReconcilers(t *testing.T) {
	now := &clock{now: time.Now()}
	c := start(t, now)
	ctx := context.Background()

	post := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.1"}}
	if err := c.Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	// feedback-sync's GitHub fields: two 👍, one from u1's linked account.
	post.Status.Votes = v1.PostVotes{GitHub: 2, GitHubReactions: []v1.GitHubReaction{{Login: "gh-u1"}, {Login: "someone"}}}
	if err := c.Status().Update(ctx, post); err != nil {
		t.Fatal(err)
	}

	t.Run("a vote follows the post and records the voter", func(t *testing.T) {
		if err := c.Create(ctx, newVote(post.Name, "u1")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "subscription", func() error {
			var s v1.Subscription
			if err := c.Get(ctx, types.NamespacedName{Name: v1.OwnedName(post.Name, "u1")}, &s); err != nil {
				return err
			}
			if s.Spec.Reason != v1.SubscriptionReasonVoted {
				return errorf("reason " + string(s.Spec.Reason))
			}
			return nil
		})
		eventually(t, "vote status", func() error {
			var v v1.Vote
			if err := c.Get(ctx, types.NamespacedName{Name: v1.OwnedName(post.Name, "u1")}, &v); err != nil {
				return err
			}
			if !v.Status.Subscribed || v.Status.GitHubLogin != "gh-u1" || len(v.Status.Organizations) != 1 {
				return errorf("status not filled in")
			}
			return nil
		})
	})

	t.Run("a deleted follow isn't recreated", func(t *testing.T) {
		s := &v1.Subscription{ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post.Name, "u1")}}
		if err := c.Delete(ctx, s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := c.Get(ctx, client.ObjectKeyFromObject(s), s); !apierrors.IsNotFound(err) {
			t.Errorf("follow came back: %v", err)
		}
	})

	t.Run("totals count linked people once", func(t *testing.T) {
		if err := c.Create(ctx, newVote(post.Name, "u2")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "totals", func() error {
			var p v1.Post
			if err := c.Get(ctx, client.ObjectKeyFromObject(post), &p); err != nil {
				return err
			}
			// 2 Datum votes + 2 👍 - 1 person who did both.
			v := p.Status.Votes
			if v.Datum != 2 || v.Linked != 1 || v.Total != 3 || v.TrendingScore == 0 {
				return errorf("votes not tallied yet")
			}
			// Sync's fields are untouched.
			if v.GitHub != 2 || len(v.GitHubReactions) != 2 {
				return errorf("sync's fields changed")
			}
			return nil
		})
	})

	t.Run("votes and follows move off a merged post", func(t *testing.T) {
		target := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.2"}}
		if err := c.Create(ctx, target); err != nil {
			t.Fatal(err)
		}
		// u2 already voted on the target, so keeps that vote.
		if err := c.Create(ctx, newVote(target.Name, "u2")); err != nil {
			t.Fatal(err)
		}
		var p v1.Post
		if err := c.Get(ctx, client.ObjectKeyFromObject(post), &p); err != nil {
			t.Fatal(err)
		}
		p.Status.MergedInto = target.Name
		if err := c.Status().Update(ctx, &p); err != nil {
			t.Fatal(err)
		}
		eventually(t, "moved", func() error {
			var left v1.VoteList
			if err := c.List(ctx, &left); err != nil {
				return err
			}
			names := map[string]bool{}
			for _, v := range left.Items {
				names[v.Name] = true
			}
			if names[v1.OwnedName(post.Name, "u1")] || names[v1.OwnedName(post.Name, "u2")] {
				return errorf("votes still on the merged post")
			}
			if !names[v1.OwnedName(target.Name, "u1")] || !names[v1.OwnedName(target.Name, "u2")] {
				return errorf("votes not on the target")
			}
			return nil
		})
		eventually(t, "target totals", func() error {
			var tp v1.Post
			if err := c.Get(ctx, client.ObjectKeyFromObject(target), &tp); err != nil {
				return err
			}
			if tp.Status.Votes.Datum != 2 {
				return errorf("target not tallied")
			}
			return nil
		})
	})

	t.Run("votes on a deleted post go after the retention period", func(t *testing.T) {
		gone := &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "datum-cloud.enhancements.3"}}
		if err := c.Create(ctx, gone); err != nil {
			t.Fatal(err)
		}
		v := newVote(gone.Name, "u3")
		if err := c.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, gone); err != nil {
			t.Fatal(err)
		}
		eventually(t, "orphan marked", func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(v), v); err != nil {
				return err
			}
			if v.Annotations[OrphanedAtAnnotation] == "" {
				return errorf("not marked")
			}
			return nil
		})
		// Past the retention period, a touch re-queues it and it goes.
		now.Set(time.Now().Add(OrphanRetention + time.Hour))
		patch := client.MergeFrom(v.DeepCopy())
		v.Labels = map[string]string{"touch": "1"}
		if err := c.Patch(ctx, v, patch); err != nil {
			t.Fatal(err)
		}
		eventually(t, "orphan deleted", func() error {
			err := c.Get(ctx, client.ObjectKeyFromObject(v), &v1.Vote{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return errorf("still there")
		})
	})
}
