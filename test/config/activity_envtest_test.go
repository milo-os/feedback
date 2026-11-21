package config

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// auditPolicy logs feedback writes at RequestResponse, as Milo's audit
// policy does for votes and subscriptions.
const auditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
  - level: RequestResponse
    resources:
      - group: feedback.miloapis.com
  - level: None
`

// TestDeleteActivityInAnAPIServer deletes a vote and a subscription in an
// envtest API server with the CRDs, post-ref.yaml and audit logging, and runs
// the audit events it writes through the activity policies. It checks the
// delete event has no spec, only a Status, and that the entries name the
// post from post-ref.yaml's audit annotation.
func TestDeleteActivityInAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run with task test")
	}
	dir := t.TempDir()
	policyFile, logFile := filepath.Join(dir, "policy.yaml"), filepath.Join(dir, "audit.log")
	if err := os.WriteFile(policyFile, []byte(auditPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	env.ControlPlane.GetAPIServer().Configure().
		Set("audit-policy-file", policyFile).
		Set("audit-log-path", logFile).
		Set("audit-log-format", "json").
		Set("audit-log-mode", "blocking")
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range load(t, "../../config/milo/activity") {
		if k := o.str("kind"); k == "ValidatingAdmissionPolicy" || k == "ValidatingAdmissionPolicyBinding" {
			if err := admin.Create(ctx, &unstructured.Unstructured{Object: o}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A person, not a system: user, so the rules don't leave them out.
	alice, err := env.AddUser(envtest.User{Name: "alice@example.com", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(alice.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	const post, user = "datum-cloud.enhancements.912", "312983457298312345"
	ps := policies(t)
	for _, tc := range []struct {
		plural, summary string
		obj             func() client.Object
	}{
		{"votes", "alice@example.com removed their vote on " + post, func() client.Object {
			return &v1.Vote{
				ObjectMeta: metaName(v1.OwnedName(post, user)),
				Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: user}, Source: v1.ClientWebsite},
			}
		}},
		{"subscriptions", "alice@example.com stopped following " + post, func() client.Object {
			return &v1.Subscription{
				ObjectMeta: metaName(v1.OwnedName(post, user)),
				Spec:       v1.SubscriptionSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: user}, Reason: v1.SubscriptionReasonManual},
			}
		}},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			// The policy takes effect once the API server has loaded it, so
			// try a few times.
			var event map[string]any
			for i := 0; i < 30; i++ {
				o := tc.obj()
				if err := as.Create(ctx, o); err != nil {
					t.Fatal(err)
				}
				if err := as.Delete(ctx, o); err != nil {
					t.Fatal(err)
				}
				event = lastDelete(t, logFile, tc.plural)
				if event != nil && obj(event).str("annotations", postRefAnnotation) != "" {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if event == nil {
				t.Fatal("no delete audit event")
			}
			checkDeleteEventShape(t, event)
			if got := obj(event).str("annotations", postRefAnnotation); got != post {
				t.Fatalf("annotation %s = %q, want %q", postRefAnnotation, got, post)
			}
			got, ok := render(t, ps[tc.plural], event)
			if !ok || got.summary != tc.summary {
				t.Errorf("rendered %q (rule %s, matched %v), want %q", got.summary, got.rule, ok, tc.summary)
			}
		})
	}
}

// lastDelete returns the last completed delete of plural in the audit log.
func lastDelete(t *testing.T, path, plural string) map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var last map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if obj(e).str("verb") == "delete" && obj(e).str("stage") == "ResponseComplete" && obj(e).str("objectRef", "resource") == plural {
			last = e
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return last
}

func metaName(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }
