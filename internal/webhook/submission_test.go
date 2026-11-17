package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/index"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestSubmissionCheck(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	sent := func(name, user string, ago time.Duration) *v1.Submission {
		return &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-ago))},
			Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: user}}}
	}
	subs := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&v1.Submission{}, index.Owner, func(o client.Object) []string { return []string{o.(*v1.Submission).Spec.UserRef.Name} }).
		WithObjects(sent("a", "u1", time.Minute), sent("b", "u1", 2*time.Minute), sent("c", "u1", time.Hour), sent("d", "u2", time.Minute)).
		Build()
	c := SubmissionCheck{Submissions: subs, Limit: 2, Window: 10 * time.Minute, Now: func() time.Time { return now }}

	user := func(id, verified string) authenticationv1.UserInfo {
		u := inUserContext(id)
		u.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{verified}
		return u
	}
	newSub := func(owner string, attachments int) *v1.Submission {
		s := &v1.Submission{Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: owner}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b"}}
		for range attachments {
			s.Spec.Attachments = append(s.Spec.Attachments, v1.AttachmentReference{UploadRef: "x"})
		}
		return s
	}

	resp := c.Handle(context.Background(), request(t, admissionv1.Create, "submissions", user("u1", "true"), newSub("u1", 0), nil))
	if resp.Allowed || resp.Result.Code != http.StatusTooManyRequests || !strings.Contains(resp.Result.Message, "try again in 8m") {
		t.Errorf("third in ten minutes: allowed %v, %d %q", resp.Allowed, resp.Result.Code, resp.Result.Message)
	}
	if resp := c.Handle(context.Background(), request(t, admissionv1.Create, "submissions", user("u2", "true"), newSub("u2", 0), nil)); !resp.Allowed {
		t.Errorf("second in ten minutes denied: %v", resp.Result.Message)
	}
	if resp := c.Handle(context.Background(), request(t, admissionv1.Create, "submissions", user("u2", "false"), newSub("u2", 0), nil)); resp.Allowed {
		t.Error("unverified email allowed")
	}
	if resp := c.Handle(context.Background(), request(t, admissionv1.Create, "submissions", user("u2", "true"), newSub("u2", 1), nil)); resp.Allowed {
		t.Error("attachments allowed without uploads")
	}
	c.Limit = 0
	if resp := c.Handle(context.Background(), request(t, admissionv1.Create, "submissions", user("u1", "true"), newSub("u1", 0), nil)); !resp.Allowed {
		t.Error("limit 0 still limits")
	}
}

func TestReviewCheck(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	posts := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "open"}},
		&v1.Post{ObjectMeta: metav1.ObjectMeta{Name: "hidden"}, Spec: v1.PostSpec{Hidden: true}},
	).Build()
	c := ReviewCheck{Posts: posts}
	staff := authenticationv1.UserInfo{Username: "staff@datum.net"}
	review := func(d v1.ReviewDecision, into string) *v1.SubmissionReview {
		return &v1.SubmissionReview{Spec: v1.SubmissionReviewSpec{Decision: d, MergedInto: into, Reason: "r"}}
	}
	for _, tc := range []struct {
		name    string
		obj     *v1.SubmissionReview
		allowed bool
	}{
		{"merge into a visible post", review(v1.ReviewDecisionMerged, "open"), true},
		{"merge into a hidden post", review(v1.ReviewDecisionMerged, "hidden"), false},
		{"merge into a missing post", review(v1.ReviewDecisionMerged, "nope"), false},
		{"reject", review(v1.ReviewDecisionRejected, ""), true},
	} {
		resp := c.Handle(context.Background(), request(t, admissionv1.Update, "submissionreviews", staff, tc.obj, review("", "")))
		if resp.Allowed != tc.allowed {
			t.Errorf("%s: allowed %v (%v)", tc.name, resp.Allowed, resp.Result.Message)
		}
	}
}

// TestSubmissionLimitHoldsForParallelRequests checks that requests racing the
// cache on one replica can't all get through: the cache shows none of them.
func TestSubmissionLimitHoldsForParallelRequests(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	subs := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&v1.Submission{}, index.Owner, func(o client.Object) []string { return []string{o.(*v1.Submission).Spec.UserRef.Name} }).
		Build()
	c := SubmissionCheck{Submissions: subs, Limit: 3, Window: 10 * time.Minute, Now: func() time.Time { return now }, Admitted: NewAdmitted()}
	u := inUserContext("u1")
	u.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{"true"}
	sub := &v1.Submission{Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b"}}
	req := request(t, admissionv1.Create, "submissions", u, sub, nil)

	var wg sync.WaitGroup
	var allowed atomic.Int32
	for range 20 {
		wg.Go(func() {
			if c.Handle(context.Background(), req).Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 3 {
		t.Errorf("%d of 20 parallel submissions allowed, want 3", got)
	}

	// Once the cache shows the admitted submissions, they aren't counted
	// twice.
	for i := range 3 {
		_ = subs.Create(context.Background(), &v1.Submission{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("s%d", i), CreationTimestamp: metav1.NewTime(now.Add(time.Second))},
			Spec:       v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}},
		})
	}
	var cached v1.SubmissionList
	if err := subs.List(context.Background(), &cached); err != nil || len(cached.Items) != 3 || !cached.Items[0].CreationTimestamp.Equal(&metav1.Time{Time: now.Add(time.Second)}) {
		t.Fatalf("cache fixture: %v %+v", err, cached.Items)
	}
	c.Limit = 4
	c.Now = func() time.Time { return now.Add(2 * time.Second) }
	if !c.Handle(context.Background(), req).Allowed {
		t.Error("fourth submission denied with a limit of 4")
	}
	if c.Handle(context.Background(), req).Allowed {
		t.Error("fifth submission allowed with a limit of 4")
	}
}

// TestSubmissionLimitCountsSameSecondOnce covers an admission and its
// submission in the same second: creationTimestamp has whole seconds, so the
// submission looks older than its own admission, and it must not count
// twice.
func TestSubmissionLimitCountsSameSecondOnce(t *testing.T) {
	second := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	subs := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&v1.Submission{}, index.Owner, func(o client.Object) []string { return []string{o.(*v1.Submission).Spec.UserRef.Name} }).
		Build()
	now := second.Add(500 * time.Millisecond)
	c := SubmissionCheck{Submissions: subs, Limit: 2, Window: 10 * time.Minute, Now: func() time.Time { return now }, Admitted: NewAdmitted()}
	u := inUserContext("u1")
	u.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{"true"}
	req := request(t, admissionv1.Create, "submissions", u,
		&v1.Submission{Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b"}}, nil)

	if !c.Handle(context.Background(), req).Allowed {
		t.Fatal("first submission denied")
	}
	// The API server stores it with the second the admission happened in.
	if err := subs.Create(context.Background(), &v1.Submission{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", CreationTimestamp: metav1.NewTime(second)},
		Spec:       v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}},
	}); err != nil {
		t.Fatal(err)
	}
	now = second.Add(700 * time.Millisecond)
	if !c.Handle(context.Background(), req).Allowed {
		t.Error("second submission denied with a limit of 2: the first counted twice")
	}
	if c.Handle(context.Background(), req).Allowed {
		t.Error("third submission allowed with a limit of 2")
	}
}
