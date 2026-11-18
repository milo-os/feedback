package controller

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/webhook"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// withWebhooks is an API server with the generated admission webhooks
// installed, and one manager serving them and running the reconcilers as
// the operator, as in production.
type withWebhooks struct {
	// admin reads everything and plays feedback-sync and staff.
	admin client.Client
	cfg   *rest.Config
	// denied counts the operator's creates that admission refused.
	denied *atomic.Int64
}

// startWithWebhooks starts the reconcilers and the admission webhooks
// together, so the reconcilers' writes go through the same admission checks
// as in Milo.
func startWithWebhooks(t *testing.T) *withWebhooks {
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
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "milo", "webhook", "manifests.yaml")},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	// The operator presents its own identity, which the owner webhook trusts
	// to name a vote's owner.
	operator := rest.CopyConfig(cfg)
	operator.Impersonate = rest.ImpersonationConfig{UserName: identity.OperatorUser, Groups: []string{"system:masters"}}
	denied := &atomic.Int64{}
	o := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(operator, ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr(true)},
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Host: o.LocalServingHost, Port: o.LocalServingPort, CertDir: o.LocalServingCertDir,
		}),
		NewClient: func(cfg *rest.Config, opts client.Options) (client.Client, error) {
			c, err := client.NewWithWatch(cfg, opts)
			if err != nil {
				return nil, err
			}
			return interceptor.NewClient(c, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					err := c.Create(ctx, obj, opts...)
					if apierrors.IsForbidden(err) {
						denied.Add(1)
					}
					return err
				},
			}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	webhook.Register(mgr)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := Setup(ctx, mgr, Options{Users: fakeUsers{}}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	check := mgr.GetWebhookServer().StartedChecker()
	eventually(t, "webhook server", func() error { return check(nil) })

	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return &withWebhooks{admin: admin, cfg: cfg, denied: denied}
}

// as returns a client acting in user's own context.
func (e *withWebhooks) as(t *testing.T, user string) client.Client {
	t.Helper()
	c := rest.CopyConfig(e.cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: user + "@example.com",
		Groups:   []string{"system:masters"},
		Extra:    map[string][]string{identity.ParentKindExtraKey: {identity.ParentKindUser}, identity.ParentNameExtraKey: {user}},
	}
	cl, err := client.New(c, client.Options{Scheme: e.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// TestMergesWithAdmissionWebhooks checks merges with the admission webhooks
// in the path: the webhook refuses votes and follows on merged or hidden
// posts, so the reconciler must never try to put them there.
func TestMergesWithAdmissionWebhooks(t *testing.T) {
	e := startWithWebhooks(t)
	c := e.admin
	ctx := context.Background()

	newPost := func(t *testing.T, name string) {
		t.Helper()
		if err := c.Create(ctx, &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	// vote votes as user and waits for the operator to follow the post for
	// them. The webhook reads posts from the operator's cache, which catches
	// up with a new post shortly.
	vote := func(t *testing.T, post, user string) {
		t.Helper()
		as := e.as(t, user)
		eventually(t, "vote on "+post, func() error {
			err := as.Create(ctx, newVote(post, user))
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return err
		})
		eventually(t, "follow of "+post, func() error {
			return c.Get(ctx, types.NamespacedName{Name: v1.OwnedName(post, user)}, &v1.Subscription{})
		})
	}
	updatePost := func(t *testing.T, name string, change func(*v1.Post), status bool) {
		t.Helper()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var p v1.Post
			if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
				return err
			}
			change(&p)
			if status {
				return c.Status().Update(ctx, &p)
			}
			return c.Update(ctx, &p)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	merge := func(t *testing.T, from, into string) {
		t.Helper()
		updatePost(t, from, func(p *v1.Post) { p.Status.MergedInto = into }, true)
	}
	hide := func(t *testing.T, name string, hidden bool) {
		t.Helper()
		updatePost(t, name, func(p *v1.Post) { p.Spec.Hidden = hidden }, false)
	}
	// owners returns who has a vote and who has a follow on post.
	owners := func(post string) (votes, follows string, err error) {
		var vl v1.VoteList
		if err := c.List(ctx, &vl); err != nil {
			return "", "", err
		}
		var sl v1.SubscriptionList
		if err := c.List(ctx, &sl); err != nil {
			return "", "", err
		}
		var vs, ss []string
		for _, v := range vl.Items {
			if v.Spec.PostRef.Name == post && v.DeletionTimestamp == nil {
				vs = append(vs, v.Spec.UserRef.Name)
			}
		}
		for _, s := range sl.Items {
			if s.Spec.PostRef.Name == post && s.DeletionTimestamp == nil {
				ss = append(ss, s.Spec.UserRef.Name)
			}
		}
		sort.Strings(vs)
		sort.Strings(ss)
		return strings.Join(vs, ","), strings.Join(ss, ","), nil
	}
	expectOwners := func(post, votes, follows string) func() error {
		return func() error {
			v, f, err := owners(post)
			if err != nil {
				return err
			}
			if v != votes || f != follows {
				return errorf(post + ": votes [" + v + "] follows [" + f + "], want [" + votes + "] [" + follows + "]")
			}
			return nil
		}
	}
	expectCondition := func(post string, status metav1.ConditionStatus, reason, inMessage string) func() error {
		return func() error {
			var p v1.Post
			if err := c.Get(ctx, types.NamespacedName{Name: post}, &p); err != nil {
				return err
			}
			cond := meta.FindStatusCondition(p.Status.Conditions, v1.PostConditionVotesMoved)
			if cond == nil {
				return errorf(post + ": no " + v1.PostConditionVotesMoved + " condition")
			}
			if cond.Status != status || cond.Reason != reason || !strings.Contains(cond.Message, inMessage) {
				return errorf(post + ": condition " + string(cond.Status) + " " + cond.Reason + ": " + cond.Message)
			}
			return nil
		}
	}
	// noRetries checks the operator stops trying to create anything
	// admission refuses.
	noRetries := func(t *testing.T) {
		t.Helper()
		before := e.denied.Load()
		time.Sleep(2 * time.Second)
		if after := e.denied.Load(); after != before {
			t.Errorf("the operator kept retrying: %d creates refused in 2s", after-before)
		}
	}

	t.Run("votes follow a merge into a post that is already merged", func(t *testing.T) {
		a, b, final := "o.r.1", "o.r.2", "o.r.3"
		newPost(t, a)
		newPost(t, b)
		newPost(t, final)
		vote(t, a, "u1")
		vote(t, b, "u2")
		vote(t, final, "u2")

		merge(t, b, final)
		eventually(t, "b's votes on the final post", expectOwners(final, "u2", "u2"))
		merge(t, a, b)

		eventually(t, "a's votes on the final post", expectOwners(final, "u1,u2", "u1,u2"))
		eventually(t, "nothing left on a", expectOwners(a, "", ""))
		eventually(t, "nothing left on b", expectOwners(b, "", ""))
		eventually(t, "a's condition", expectCondition(a, metav1.ConditionTrue, v1.PostReasonMoved, final))
		eventually(t, "b's condition", expectCondition(b, metav1.ConditionTrue, v1.PostReasonMoved, final))
		eventually(t, "final totals", func() error {
			var p v1.Post
			if err := c.Get(ctx, types.NamespacedName{Name: final}, &p); err != nil {
				return err
			}
			if p.Status.Votes.Datum != 2 {
				return errorf("final post not tallied")
			}
			return nil
		})
	})

	t.Run("votes stay on a post merged into a hidden one until it's shown", func(t *testing.T) {
		from, hidden := "o.r.10", "o.r.11"
		newPost(t, from)
		newPost(t, hidden)
		vote(t, from, "u1")
		hide(t, hidden, true)
		merge(t, from, hidden)

		eventually(t, "condition", expectCondition(from, metav1.ConditionFalse, v1.PostReasonTargetUnavailable, hidden))
		noRetries(t)
		if err := expectOwners(from, "u1", "u1")(); err != nil {
			t.Fatal(err)
		}
		if err := expectOwners(hidden, "", "")(); err != nil {
			t.Fatal(err)
		}

		hide(t, hidden, false)
		eventually(t, "votes moved once the post is shown", expectOwners(hidden, "u1", "u1"))
		eventually(t, "nothing left", expectOwners(from, "", ""))
		eventually(t, "condition", expectCondition(from, metav1.ConditionTrue, v1.PostReasonMoved, hidden))
	})

	t.Run("votes stay on a post merged into one that doesn't exist yet", func(t *testing.T) {
		from, missing := "o.r.20", "o.r.21"
		newPost(t, from)
		vote(t, from, "u1")
		merge(t, from, missing)

		eventually(t, "condition", expectCondition(from, metav1.ConditionFalse, v1.PostReasonTargetUnavailable, missing))
		noRetries(t)
		if err := expectOwners(from, "u1", "u1")(); err != nil {
			t.Fatal(err)
		}

		newPost(t, missing)
		eventually(t, "votes moved once the post exists", expectOwners(missing, "u1", "u1"))
		eventually(t, "condition", expectCondition(from, metav1.ConditionTrue, v1.PostReasonMoved, missing))
	})

	t.Run("votes stay put when merges loop", func(t *testing.T) {
		p, q := "o.r.30", "o.r.31"
		newPost(t, p)
		newPost(t, q)
		vote(t, p, "u1")
		vote(t, q, "u2")
		updatePost(t, p, func(post *v1.Post) { post.Status.MergedInto = q }, true)
		updatePost(t, q, func(post *v1.Post) { post.Status.MergedInto = p }, true)

		eventually(t, "p's condition", expectCondition(p, metav1.ConditionFalse, v1.PostReasonMergeCycle, ""))
		eventually(t, "q's condition", expectCondition(q, metav1.ConditionFalse, v1.PostReasonMergeCycle, ""))
		noRetries(t)
		// p may have moved its votes to q before q was merged back, but
		// none are lost.
		pv, _, err := owners(p)
		if err != nil {
			t.Fatal(err)
		}
		qv, _, err := owners(q)
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Split(pv+","+qv, ",")
		sort.Strings(all)
		if got := strings.Trim(strings.Join(all, ","), ","); got != "u1,u2" {
			t.Errorf("votes p [%s] q [%s], want u1 and u2 once each", pv, qv)
		}

		// Breaking the loop moves them, and the unmerged post loses its
		// condition.
		updatePost(t, q, func(post *v1.Post) { post.Status.MergedInto = "" }, true)
		eventually(t, "votes moved once the loop is broken", expectOwners(q, "u1,u2", "u1,u2"))
		eventually(t, "p's condition", expectCondition(p, metav1.ConditionTrue, v1.PostReasonMoved, q))
		eventually(t, "q's condition removed", func() error {
			var post v1.Post
			if err := c.Get(ctx, types.NamespacedName{Name: q}, &post); err != nil {
				return err
			}
			if meta.FindStatusCondition(post.Status.Conditions, v1.PostConditionVotesMoved) != nil {
				return errorf("still there")
			}
			return nil
		})
	})
}
