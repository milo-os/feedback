package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// newAPIServer starts an API server with the generated CRDs installed. It
// needs KUBEBUILDER_ASSETS (task test sets it) and skips without it.
func newAPIServer(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run with task test")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: newScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCRDs(t *testing.T) {
	c := newAPIServer(t)
	ctx := context.Background()

	t.Run("every sample is accepted", func(t *testing.T) {
		scheme := newScheme(t)
		decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
		files, _ := filepath.Glob("../../../../config/samples/*.yaml")
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			obj, _, err := decoder.Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if err := c.Create(ctx, obj.(client.Object)); err != nil {
				t.Errorf("%s: %v", filepath.Base(f), err)
			}
		}
	})

	t.Run("vote names are post.user", func(t *testing.T) {
		v := vote("p1", "u1")
		v.Name = "p1"
		expectInvalid(t, c.Create(ctx, v), "metadata.name must be")
		if err := c.Create(ctx, vote("p1", "u1")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("vote spec is immutable", func(t *testing.T) {
		v := vote("p2", "u1")
		if err := c.Create(ctx, v); err != nil {
			t.Fatal(err)
		}
		v.Spec.Source = v1.ClientPortal
		expectInvalid(t, c.Update(ctx, v), "spec is immutable")
	})

	t.Run("field selectors on owner and post", func(t *testing.T) {
		if err := c.Create(ctx, vote("p1", "u2")); err != nil {
			t.Fatal(err)
		}
		var list v1.VoteList
		if err := c.List(ctx, &list, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.userRef.name", "u2")}); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 1 || list.Items[0].Spec.UserRef.Name != "u2" {
			t.Errorf("by user: %d votes", len(list.Items))
		}
		if err := c.List(ctx, &list, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.postRef.name", "p1")}); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 2 {
			t.Errorf("by post: %d votes, want 2", len(list.Items))
		}
	})

	t.Run("hidden posts can be filtered out", func(t *testing.T) {
		for name, hidden := range map[string]bool{"a.b.1": false, "a.b.2": true} {
			if err := c.Create(ctx, &v1.Post{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.PostSpec{Hidden: hidden}}); err != nil {
				t.Fatal(err)
			}
		}
		var list v1.PostList
		if err := c.List(ctx, &list, client.MatchingFieldsSelector{Selector: fields.OneTermNotEqualSelector("spec.hidden", "true")}); err != nil {
			t.Fatal(err)
		}
		for _, p := range list.Items {
			if p.Spec.Hidden {
				t.Errorf("hidden post %s listed", p.Name)
			}
		}
	})

	t.Run("a post created without spec is served with one", func(t *testing.T) {
		// The search index condition reads spec.hidden and can't evaluate it
		// on an object with no spec, so the CRD defaults spec to {}.
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": v1.SchemeGroupVersion.String(),
			"kind":       "Post",
			"metadata":   map[string]any{"name": "a.b.no-spec"},
		}}
		if err := c.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.Object["spec"].(map[string]any); !ok {
			t.Errorf("create response has no spec: %v", u.Object)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("Post"))
		if err := c.Get(ctx, client.ObjectKey{Name: "a.b.no-spec"}, got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Object["spec"].(map[string]any); !ok {
			t.Errorf("stored post has no spec: %v", got.Object)
		}
	})

	t.Run("review decisions", func(t *testing.T) {
		r := &v1.SubmissionReview{ObjectMeta: metav1.ObjectMeta{Name: "sub-1"}}
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Spec.Decision = v1.ReviewDecisionApproved
		expectInvalid(t, c.Update(ctx, r), "Approved needs publicTitle and publicBody")
		r.Spec.Decision = v1.ReviewDecisionRejected
		expectInvalid(t, c.Update(ctx, r), "Rejected needs a reason")
		r.Spec.Reason = "Out of scope"
		if err := c.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Spec.Decision = v1.ReviewDecisionMerged
		r.Spec.MergedInto = "a.b.1"
		expectInvalid(t, c.Update(ctx, r), "can't change once it is set")
	})

	t.Run("submissions can't change", func(t *testing.T) {
		s := &v1.Submission{
			ObjectMeta: metav1.ObjectMeta{Name: "sub-2"},
			Spec:       v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b"},
		}
		if err := c.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		s.Spec.Title = "changed"
		expectInvalid(t, c.Update(ctx, s), "can't be changed after they are sent")
	})

	t.Run("feedback sources are named after their org", func(t *testing.T) {
		src := func(name, org string) *v1.FeedbackSource {
			return &v1.FeedbackSource{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: v1.FeedbackSourceSpec{GitHub: v1.GitHubSource{
					Org: org, InstallationID: 1, Issues: v1.IssueSelector{Labels: []string{"Build"}},
				}},
			}
		}
		expectInvalid(t, c.Create(ctx, src("other", "Example-Org")), "one source per org")
		if err := c.Create(ctx, src("example-org", "Example-Org")); err != nil {
			t.Fatal(err)
		}
		empty := src("empty-org", "empty-org")
		empty.Spec.GitHub.Issues = v1.IssueSelector{ExcludeLabels: []string{"feedback:hide"}}
		expectInvalid(t, c.Create(ctx, empty), "at least one issue type or label")
		quoted := src("quoted-org", "quoted-org")
		quoted.Spec.GitHub.Issues.Labels = []string{`say "hi"`}
		expectInvalid(t, c.Create(ctx, quoted), "spec.github.issues.labels")
		glob := src("glob-org", "glob-org")
		glob.Spec.GitHub.Repositories.Exclude = []string{"foo label:x"}
		expectInvalid(t, c.Create(ctx, glob), "spec.github.repositories.exclude")
	})

	t.Run("profiles are named after their user", func(t *testing.T) {
		p := &v1.FeedbackProfile{ObjectMeta: metav1.ObjectMeta{Name: "u9"}, Spec: v1.FeedbackProfileSpec{UserRef: v1.UserReference{Name: "u8"}}}
		expectInvalid(t, c.Create(ctx, p), "must be the user's name")
	})
}

func vote(post, user string) *v1.Vote {
	return &v1.Vote{
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName(post, user)},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: post}, UserRef: v1.UserReference{Name: user}, Source: v1.ClientWebsite},
	}
}

func expectInvalid(t *testing.T, err error, msg string) {
	t.Helper()
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), msg) {
		t.Errorf("err = %v, want Invalid containing %q", err, msg)
	}
}
