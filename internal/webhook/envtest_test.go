package webhook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// TestWebhooksInFrontOfAnAPIServer installs the CRDs and the generated
// webhook config in an envtest API server, serves the webhooks, and makes
// requests as users in their own context. It checks the mutation and the
// CRDs' own rules work together: the vote name rule runs after the owner is
// set.
func TestWebhooksInFrontOfAnAPIServer(t *testing.T) {
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

	o := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Host: o.LocalServingHost, Port: o.LocalServingPort, CertDir: o.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	Register(mgr)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	waitForWebhooks(t, mgr)

	as := func(user string, extra map[string][]string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters"}, Extra: extra}
		cl, err := client.New(c, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	ada := as("ada@example.com", map[string][]string{identity.ParentKindExtraKey: {"User"}, identity.ParentNameExtraKey: {"u1"}})
	staff := as("staff@datum.net", nil)

	vote := func(name, owner string) *v1.Vote {
		return &v1.Vote{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: "p1"}, UserRef: v1.UserReference{Name: owner}},
		}
	}

	t.Run("the caller becomes the owner", func(t *testing.T) {
		v := vote("p1.u1", "")
		if err := ada.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
		if v.Spec.UserRef.Name != "u1" {
			t.Errorf("owner = %q", v.Spec.UserRef.Name)
		}
	})

	t.Run("someone else's name is replaced, so the name rule fails", func(t *testing.T) {
		err := ada.Create(ctx, vote("p1.u2", "u2"))
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "metadata.name must be") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("staff can't vote for users", func(t *testing.T) {
		if err := staff.Create(ctx, vote("p1.u3", "u3")); !apierrors.IsForbidden(err) {
			t.Errorf("err = %v, want Forbidden", err)
		}
	})

	t.Run("the reviewer is recorded", func(t *testing.T) {
		r := &v1.SubmissionReview{ObjectMeta: metav1.ObjectMeta{Name: "sub-1"}}
		if err := staff.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Spec = v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "dup", Reviewer: "someone-else"}
		if err := staff.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
		if r.Spec.Reviewer != "staff@datum.net" || r.Spec.ReviewedAt == nil {
			t.Errorf("reviewer %q at %v", r.Spec.Reviewer, r.Spec.ReviewedAt)
		}
	})
}

// waitForWebhooks waits until the webhook server answers.
func waitForWebhooks(t *testing.T, mgr ctrl.Manager) {
	t.Helper()
	check := mgr.GetWebhookServer().StartedChecker()
	deadline := time.Now().Add(20 * time.Second)
	for check(nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("webhook server didn't start")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
