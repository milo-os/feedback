package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

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

// limitFixture is a SubmissionCheck with an empty cache and a record of
// admissions, and a way to make requests from u1.
type limitFixture struct {
	t    *testing.T
	subs client.Client
	c    SubmissionCheck
	now  time.Time
}

func newLimitFixture(t *testing.T, limit int, now time.Time) *limitFixture {
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	f := &limitFixture{t: t, now: now}
	f.subs = fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&v1.Submission{}, index.Owner, func(o client.Object) []string { return []string{o.(*v1.Submission).Spec.UserRef.Name} }).
		Build()
	f.c = SubmissionCheck{Submissions: f.subs, Limit: limit, Window: 10 * time.Minute, Now: func() time.Time { return f.now }, Admitted: NewAdmitted()}
	return f
}

// req is a create of a submission with uid, as the API server sends it to
// validating admission: the UID is already set.
func (f *limitFixture) req(uid string, dryRun bool) admission.Request {
	u := inUserContext("u1")
	u.Extra[identity.EmailVerifiedExtraKey] = authenticationv1.ExtraValue{"true"}
	sub := &v1.Submission{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)},
		Spec: v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}, Kind: v1.SubmissionKindIdea, Title: "t", Body: "b"}}
	r := request(f.t, admissionv1.Create, "submissions", u, sub, nil)
	if dryRun {
		r.DryRun = ptr.To(true)
	}
	return r
}

func (f *limitFixture) allowed(uid string) bool {
	return f.c.Handle(context.Background(), f.req(uid, false)).Allowed
}

// store puts a submission in the cache, as the API server stored it.
func (f *limitFixture) store(uid string, created time.Time) {
	f.t.Helper()
	if err := f.subs.Create(context.Background(), &v1.Submission{
		ObjectMeta: metav1.ObjectMeta{Name: "s-" + uid, UID: types.UID(uid), CreationTimestamp: metav1.NewTime(created)},
		Spec:       v1.SubmissionSpec{UserRef: v1.UserReference{Name: "u1"}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

// TestSubmissionLimitHoldsForParallelRequests checks that requests racing the
// cache on one replica can't all get through: the cache shows none of them.
func TestSubmissionLimitHoldsForParallelRequests(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f := newLimitFixture(t, 3, now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var admittedUIDs []string
	for i := range 20 {
		uid := fmt.Sprintf("p%d", i)
		req := f.req(uid, false)
		wg.Go(func() {
			if f.c.Handle(context.Background(), req).Allowed {
				mu.Lock()
				admittedUIDs = append(admittedUIDs, uid)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(admittedUIDs) != 3 {
		t.Fatalf("%d of 20 parallel submissions allowed, want 3", len(admittedUIDs))
	}

	// Once the cache shows the admitted submissions, they aren't counted
	// twice.
	for _, uid := range admittedUIDs {
		f.store(uid, now)
	}
	f.c.Limit = 4
	f.now = now.Add(2 * time.Second)
	if !f.allowed("p20") {
		t.Error("fourth submission denied with a limit of 4")
	}
	if f.allowed("p21") {
		t.Error("fifth submission allowed with a limit of 4")
	}
}

// TestSubmissionLimitCountsEachSubmissionOnce covers a submission whose
// creationTimestamp is in an earlier second than its admission: the API
// server sets it before calling admission, and it has whole seconds. The
// cached submission and the record of its admission must count once.
func TestSubmissionLimitCountsEachSubmissionOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		admitted time.Time
		created  time.Time
	}{
		{"same second", time.Date(2026, 11, 17, 12, 0, 0, 500e6, time.UTC), time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)},
		{"across a second boundary", time.Date(2026, 11, 17, 12, 0, 1, 20e6, time.UTC), time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLimitFixture(t, 2, tc.admitted)
			if !f.allowed("a") {
				t.Fatal("first submission denied")
			}
			f.store("a", tc.created)
			f.now = tc.admitted.Add(200 * time.Millisecond)
			if !f.allowed("b") {
				t.Error("second submission denied with a limit of 2: the first counted twice")
			}
			if f.allowed("c") {
				t.Error("third submission allowed with a limit of 2")
			}
		})
	}
}

// TestSubmissionLimitIgnoresDryRuns checks a dry run is limited like a real
// create but doesn't use up the limit, since it creates nothing.
func TestSubmissionLimitIgnoresDryRuns(t *testing.T) {
	f := newLimitFixture(t, 1, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	for range 3 {
		if !f.c.Handle(context.Background(), f.req("dry", true)).Allowed {
			t.Fatal("dry run denied")
		}
	}
	if !f.allowed("a") {
		t.Fatal("first real submission denied after dry runs")
	}
	if f.c.Handle(context.Background(), f.req("dry2", true)).Allowed {
		t.Error("dry run over the limit allowed")
	}
}

// TestSubmissionLimitForgetsFailedCreates covers a create that fails after
// admission: its submission never reaches the cache, and it stops counting
// after PendingTTL rather than for the whole window.
func TestSubmissionLimitForgetsFailedCreates(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f := newLimitFixture(t, 1, now)
	if !f.allowed("failed") {
		t.Fatal("first submission denied")
	}
	f.now = now.Add(PendingTTL - time.Second)
	if f.allowed("b") {
		t.Error("second submission allowed while the first may still be on its way")
	}
	f.now = now.Add(PendingTTL)
	if !f.allowed("b") {
		t.Error("second submission denied after the first never appeared")
	}
}

// TestSubmissionLimitCountsRepeatedAdmissionOnce covers the API server
// sending the same create to admission twice: it's one submission.
func TestSubmissionLimitCountsRepeatedAdmissionOnce(t *testing.T) {
	f := newLimitFixture(t, 1, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	if !f.allowed("a") || !f.allowed("a") {
		t.Fatal("the same create denied the second time")
	}
	if f.allowed("b") {
		t.Error("a second submission allowed with a limit of 1")
	}
}
