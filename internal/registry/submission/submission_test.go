package submission

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/ratelimit"
	"go.miloapis.com/feedback/internal/registry/attachmentupload"
	"go.miloapis.com/feedback/internal/registry/post"
	"go.miloapis.com/feedback/internal/registry/securityreport"
	"go.miloapis.com/feedback/internal/registry/vote"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	ids        = identity.Identities{Sync: []string{"sync"}, Controller: []string{"controller"}}
	syncCtx    = pgtest.AsPlatform("sync")
	controller = pgtest.AsPlatform("controller")
	staff      = pgtest.AsPlatform("staff@datum.net")
	alice      = pgtest.AsUser("alice")
	bob        = pgtest.AsUser("bob")
)

type env struct {
	db      *pg.DB
	posts   *post.REST
	votes   *vote.REST
	subs    *vote.SubscriptionREST
	s       *REST
	files   *attachments.Store
	uploads *attachmentupload.REST
}

func setup(t *testing.T) env {
	t.Helper()
	db := pgtest.Require(t, "posts", "votes", "subscriptions", "submissions", "security_reports")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	posts, err := post.NewREST(db, scheme, ids)
	if err != nil {
		t.Fatal(err)
	}
	subs, err := vote.NewSubscriptionREST(db, scheme)
	if err != nil {
		t.Fatal(err)
	}
	votes, err := vote.NewREST(db, scheme, nil, subs)
	if err != nil {
		t.Fatal(err)
	}
	files, err := attachments.Open(context.Background(), "mem://")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	s, err := NewREST(db, scheme, Options{Identities: ids, Files: files, Votes: votes, Subscriptions: subs,
		Limit: 3, Window: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return env{db, posts, votes, subs, s, files, attachmentupload.NewREST(files, nil)}
}

func (e env) addPost(t *testing.T, n int32) string {
	t.Helper()
	p := &v1.Post{
		ObjectMeta: metav1.ObjectMeta{Name: v1.PostName("datum-cloud", "enhancements", n)},
		Status: v1.PostStatus{Title: "p",
			GitHub: &v1.GitHubIssue{Org: "datum-cloud", Repository: "enhancements", Number: n, State: "open"}},
	}
	if _, err := e.posts.Create(syncCtx, p, nil, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return p.Name
}

func spec() v1.SubmissionSpec {
	return v1.SubmissionSpec{Kind: v1.SubmissionKindIdea, Title: "Pick the VPC region", Body: "Please",
		Context: v1.SubmissionContext{Client: v1.ClientPortal, Organization: "acme"}}
}

func (e env) submit(t *testing.T, ctx context.Context, s v1.SubmissionSpec) *v1.Submission {
	t.Helper()
	obj, err := e.s.Create(ctx, &v1.Submission{ObjectMeta: metav1.ObjectMeta{GenerateName: "sub-"}, Spec: s}, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return obj.(*v1.Submission)
}

func (e env) get(t *testing.T, ctx context.Context, name string) *v1.Submission {
	t.Helper()
	obj, err := e.s.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return obj.(*v1.Submission)
}

func (e env) review(ctx context.Context, name string, s v1.SubmissionReviewSpec) (*v1.Submission, error) {
	obj, err := e.s.Review.Create(ctx, name, &v1.SubmissionReview{Spec: s}, nil, &metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return obj.(*v1.Submission), nil
}

func (e env) setStatus(ctx context.Context, name string, f func(*v1.SubmissionStatus)) error {
	_, _, err := e.s.Status.Update(ctx, name, rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
		s := old.DeepCopyObject().(*v1.Submission)
		f(&s.Status)
		return s, nil
	}), nil, nil, false, &metav1.UpdateOptions{})
	return err
}

func TestCreateAndViews(t *testing.T) {
	e := setup(t)
	in := &v1.Submission{ObjectMeta: metav1.ObjectMeta{Name: "my-own-name"}, Spec: spec(),
		Status: v1.SubmissionStatus{Phase: v1.SubmissionPhaseApproved}}
	obj, err := e.s.Create(alice, in, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := obj.(*v1.Submission)
	if !strings.HasPrefix(s.Name, "sub-") || s.Name == "my-own-name" {
		t.Errorf("name %q not generated", s.Name)
	}
	if s.Status.Phase != v1.SubmissionPhasePendingReview {
		t.Errorf("phase %q", s.Status.Phase)
	}

	score := 0.1
	if err := e.setStatus(controller, s.Name, func(st *v1.SubmissionStatus) {
		st.Triage = &v1.SubmissionTriage{FraudScore: &score, SuggestedKind: v1.SubmissionKindBug}
		st.Phase = v1.SubmissionPhaseApproved // not the controller's
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, staff, s.Name); got.Status.Triage == nil || got.Status.Phase != v1.SubmissionPhasePendingReview {
		t.Errorf("staff view: %+v", got.Status)
	}
	if got := e.get(t, alice, s.Name); got.Status.Triage != nil {
		t.Error("submitter sees triage")
	}
	if _, err := e.s.Get(bob, s.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("bob reads alice's submission: %v", err)
	}
	list, err := e.s.List(alice, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if items := list.(*v1.SubmissionList).Items; len(items) != 1 || items[0].Status.Triage != nil {
		t.Errorf("alice's list: %+v", items)
	}

	for name, c := range map[string]struct {
		ctx  context.Context
		spec v1.SubmissionSpec
		want func(error) bool
	}{
		"staff":       {staff, spec(), apierrors.IsForbidden},
		"org context": {pgtest.AsContext("Organization", "acme"), spec(), apierrors.IsForbidden},
		"unverified":  {unverified("carol"), spec(), apierrors.IsForbidden},
		"no title":    {alice, func() v1.SubmissionSpec { s := spec(); s.Title = ""; return s }(), apierrors.IsInvalid},
		"bad kind":    {alice, func() v1.SubmissionSpec { s := spec(); s.Kind = "rant"; return s }(), apierrors.IsInvalid},
		"bad client":  {alice, func() v1.SubmissionSpec { s := spec(); s.Context.Client = v1.ClientEmail; return s }(), apierrors.IsInvalid},
	} {
		if _, err := e.s.Create(c.ctx, &v1.Submission{Spec: c.spec}, nil, &metav1.CreateOptions{}); !c.want(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func unverified(uid string) context.Context {
	return request.WithUser(request.WithNamespace(context.Background(), ""), &user.DefaultInfo{
		Name: uid, UID: uid, Extra: map[string][]string{
			pg.ParentKindExtraKey: {"User"}, pg.ParentNameExtraKey: {uid},
			strings.ToLower(identity.EmailVerifiedExtraKey): {"false"},
		}})
}

func TestWhoCanChangeWhat(t *testing.T) {
	e := setup(t)
	s := e.submit(t, alice, spec())

	s.Labels = map[string]string{"x": "y"}
	if _, _, err := e.s.Update(alice, s.Name, rest.DefaultUpdatedObjectInfo(s), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("submitter update: %v", err)
	}
	if _, _, err := e.s.Delete(alice, s.Name, nil, &metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("submitter delete: %v", err)
	}
	if err := e.setStatus(staff, s.Name, func(st *v1.SubmissionStatus) { st.Phase = v1.SubmissionPhaseApproved }); !apierrors.IsInvalid(err) {
		t.Errorf("staff status write: %v", err)
	}
	if err := e.setStatus(alice, s.Name, func(st *v1.SubmissionStatus) { st.Phase = v1.SubmissionPhaseApproved }); err == nil {
		t.Error("submitter status write allowed")
	}
	s2 := e.get(t, staff, s.Name)
	s2.Spec.Title = "edited"
	if _, _, err := e.s.Update(staff, s.Name, rest.DefaultUpdatedObjectInfo(s2), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("staff spec edit: %v", err)
	}
	if _, _, err := e.s.Delete(staff, s.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Errorf("staff delete: %v", err)
	}
}

func TestRejectAndRedaction(t *testing.T) {
	e := setup(t)
	s := e.submit(t, alice, spec())

	for name, c := range map[string]struct {
		ctx  context.Context
		spec v1.SubmissionReviewSpec
		want func(error) bool
	}{
		"submitter":     {alice, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "x"}, apierrors.IsForbidden},
		"controller":    {controller, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "x"}, apierrors.IsForbidden},
		"no reason":     {staff, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected}, apierrors.IsInvalid},
		"approve blank": {staff, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionApproved}, apierrors.IsInvalid},
		"bad decision":  {staff, v1.SubmissionReviewSpec{Decision: "Maybe"}, apierrors.IsInvalid},
		"missing":       {staff, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "x"}, apierrors.IsNotFound},
	} {
		target := s.Name
		if name == "missing" {
			target = "sub-nope"
		}
		if _, err := e.review(c.ctx, target, c.spec); !c.want(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}

	out, err := e.review(staff, s.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "Already possible with tags"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status.Phase != v1.SubmissionPhaseRejected || out.Status.Review.Reviewer != "staff@datum.net" {
		t.Errorf("review: %+v", out.Status)
	}
	mine := e.get(t, alice, s.Name)
	if r := mine.Status.Review; r == nil || r.Reason != "Already possible with tags" || r.Reviewer != "" || r.Decision != v1.ReviewDecisionRejected {
		t.Errorf("submitter's view of the review: %+v", r)
	}
	if _, err := e.review(staff, s.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "again"}); !apierrors.IsConflict(err) {
		t.Errorf("second review: %v", err)
	}
}

func TestMergeGivesVote(t *testing.T) {
	e := setup(t)
	p := e.addPost(t, 1)
	s := e.submit(t, alice, spec())

	if _, err := e.review(staff, s.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionMerged, MergedInto: "datum-cloud.enhancements.9"}); !apierrors.IsInvalid(err) {
		t.Fatalf("merge into a missing post: %v", err)
	}
	out, err := e.review(staff, s.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionMerged, MergedInto: p})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status.Phase != v1.SubmissionPhaseMerged || out.Status.PostRef == nil || out.Status.PostRef.Name != p {
		t.Errorf("merged status: %+v", out.Status)
	}
	v, err := e.votes.Get(alice, p, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("submitter has no vote: %v", err)
	}
	if v.(*v1.Vote).Spec.Source != v1.ClientPortal {
		t.Errorf("vote source %q", v.(*v1.Vote).Spec.Source)
	}
	sub, err := e.subs.Get(alice, p, &metav1.GetOptions{})
	if err != nil || sub.(*v1.Subscription).Spec.Reason != v1.SubscriptionReasonMerged {
		t.Errorf("subscription: %v %v", sub, err)
	}
	pp, _ := e.posts.Get(staff, p, &metav1.GetOptions{})
	if d := pp.(*v1.Post).Status.Votes.Datum; d != 1 {
		t.Errorf("post datum votes %d", d)
	}

	// A second submission from someone who already voted merges cleanly.
	s2 := e.submit(t, alice, spec())
	if _, err := e.review(staff, s2.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionMerged, MergedInto: p}); err != nil {
		t.Fatalf("merge with an existing vote: %v", err)
	}
}

func TestApproveThenSyncLinks(t *testing.T) {
	e := setup(t)
	s := e.submit(t, alice, spec())
	out, err := e.review(staff, s.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionApproved,
		PublicTitle: "Region selection for VPC attachments", PublicBody: "### Summary"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status.Phase != v1.SubmissionPhaseApproved || out.Status.PostRef != nil {
		t.Errorf("approved: %+v", out.Status)
	}
	if got := e.get(t, alice, s.Name).Status.Review; got.PublicTitle != "" {
		t.Error("submitter sees the staff-edited public text")
	}

	p := e.addPost(t, 931)
	if err := e.setStatus(syncCtx, s.Name, func(st *v1.SubmissionStatus) { st.PostRef = &v1.PostReference{Name: p} }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.votes.Get(alice, p, &metav1.GetOptions{}); err != nil {
		t.Fatalf("no vote after sync linked the post: %v", err)
	}
	sub, err := e.subs.Get(alice, p, &metav1.GetOptions{})
	if err != nil || sub.(*v1.Subscription).Spec.Reason != v1.SubscriptionReasonSubmitted {
		t.Errorf("subscription: %v %v", sub, err)
	}
	if got := e.get(t, alice, s.Name); got.Status.PostRef == nil || got.Status.PostRef.Name != p {
		t.Errorf("submitter doesn't see the post: %+v", got.Status)
	}
	if err := e.setStatus(syncCtx, s.Name, func(st *v1.SubmissionStatus) { st.PostRef = &v1.PostReference{Name: "other.repo.1"} }); !apierrors.IsInvalid(err) {
		t.Errorf("relinking: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	e := setup(t)
	for range 3 {
		e.submit(t, alice, spec())
	}
	_, err := e.s.Create(alice, &v1.Submission{Spec: spec()}, nil, &metav1.CreateOptions{})
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("4th submission: %v", err)
	}
	if d, ok := apierrors.SuggestsClientDelay(err); !ok || d < 500 || d > 600 {
		t.Errorf("retry after %d", d)
	}
	e.submit(t, bob, spec())
	if _, err := e.db.Pool.Exec(context.Background(), "UPDATE submissions SET created_at = now() - interval '11 minutes'"); err != nil {
		t.Fatal(err)
	}
	e.submit(t, alice, spec())
}

var png = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func (e env) upload(ctx context.Context, name, contentType string, data []byte) (string, error) {
	obj, err := e.uploads.Create(ctx, &v1.AttachmentUpload{Spec: v1.AttachmentUploadSpec{
		Purpose: v1.AttachmentPurposeSubmission, Name: name, ContentType: contentType, Data: data}}, nil, &metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return obj.(*v1.AttachmentUpload).Status.UploadRef, nil
}

func TestAttachments(t *testing.T) {
	e := setup(t)
	ref, err := e.upload(alice, "screen.png", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	bobRef, err := e.upload(bob, "log.txt", "text/plain", []byte("error"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		ctx               context.Context
		file, ctype, data string
		want              func(error) bool
	}{
		"svg":          {alice, "x.svg", "image/svg+xml", "<svg/>", apierrors.IsInvalid},
		"lying type":   {alice, "x.png", "image/png", "not a png", apierrors.IsInvalid},
		"path in name": {alice, "../x.txt", "text/plain", "x", apierrors.IsInvalid},
		"staff":        {staff, "x.txt", "text/plain", "x", apierrors.IsForbidden},
	} {
		if _, err := e.upload(c.ctx, c.file, c.ctype, []byte(c.data)); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}

	in := spec()
	in.Attachments = []v1.AttachmentReference{{UploadRef: bobRef}}
	if _, err := e.s.Create(alice, &v1.Submission{Spec: in}, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("someone else's upload: %v", err)
	}
	in.Attachments = []v1.AttachmentReference{{UploadRef: ref, Name: "renamed.exe", ContentType: "text/html"}}
	s := e.submit(t, alice, in)
	if a := s.Spec.Attachments[0]; a.Name != "screen.png" || a.ContentType != "image/png" {
		t.Errorf("attachment metadata from the client: %+v", a)
	}

	download := func(ctx context.Context, ref string) (*httptest.ResponseRecorder, error) {
		h, err := e.s.Attachment.Connect(ctx, s.Name, &v1.AttachmentOptions{Ref: ref}, nil)
		if err != nil {
			return nil, err
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w, nil
	}
	for _, ctx := range []context.Context{alice, staff} {
		w, err := download(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if w.Body.String() != string(png) || w.Header().Get("Content-Type") != "image/png" ||
			!strings.Contains(w.Header().Get("Content-Disposition"), `attachment; filename=screen.png`) ||
			w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("download: %v %q", w.Header(), w.Body.String())
		}
	}
	if _, err := download(bob, ref); !apierrors.IsNotFound(err) {
		t.Errorf("bob downloads: %v", err)
	}
	if _, err := download(alice, bobRef); !apierrors.IsNotFound(err) {
		t.Errorf("unattached ref: %v", err)
	}

	if _, _, err := e.s.Delete(staff, s.Name, nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.files.Get(context.Background(), v1.AttachmentPurposeSubmission, s.Name, ref); err != attachments.ErrNotFound {
		t.Errorf("attachment survived delete: %v", err)
	}
}

func TestUploadRateLimit(t *testing.T) {
	e := setup(t)
	e.uploads = attachmentupload.NewREST(e.files, ratelimit.NewLimiter(1, 1))
	if _, err := e.upload(alice, "a.txt", "text/plain", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.upload(alice, "a.txt", "text/plain", []byte("a")); !apierrors.IsTooManyRequests(err) {
		t.Errorf("second upload: %v", err)
	}
}

func TestToSecurityReport(t *testing.T) {
	e := setup(t)
	reports, err := securityreport.NewREST(e.db, func() *runtime.Scheme { s := runtime.NewScheme(); install.Install(s); return s }(),
		securityreport.Options{Identities: ids, Files: e.files})
	if err != nil {
		t.Fatal(err)
	}
	e.s.ToSecurity.reports = reports
	if _, err := e.db.Pool.Exec(context.Background(), "TRUNCATE security_reports"); err != nil {
		t.Fatal(err)
	}

	ref, err := e.upload(alice, "trace.txt", "text/plain", []byte("token=abc"))
	if err != nil {
		t.Fatal(err)
	}
	in := spec()
	in.Attachments = []v1.AttachmentReference{{UploadRef: ref}}
	s := e.submit(t, alice, in)

	move := func(ctx context.Context, name string) (*v1.SecurityReport, error) {
		obj, err := e.s.ToSecurity.Create(ctx, name, &v1.SubmissionToSecurityReport{Affected: "connectors"}, nil, &metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return obj.(*v1.SecurityReport), nil
	}
	if _, err := move(alice, s.Name); !apierrors.IsForbidden(err) {
		t.Errorf("submitter moves: %v", err)
	}
	if _, err := move(controller, s.Name); !apierrors.IsForbidden(err) {
		t.Errorf("controller moves: %v", err)
	}
	r, err := move(staff, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if r.Spec.Title != s.Spec.Title || r.Spec.Description != s.Spec.Body || r.Spec.Affected != "connectors" ||
		r.Spec.Context.Client != v1.ClientPortal || r.Annotations[pg.OwnerAnnotation] != "alice" || len(r.Spec.Attachments) != 1 {
		t.Errorf("report: %+v", r)
	}
	if _, err := e.s.Get(staff, s.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("submission still there: %v", err)
	}
	if _, err := reports.Get(alice, r.Name, &metav1.GetOptions{}); err != nil {
		t.Errorf("submitter can't read the report: %v", err)
	}
	h, err := reports.Attachment.Connect(alice, r.Name, &v1.AttachmentOptions{Ref: ref}, nil)
	if err != nil {
		t.Fatalf("attachment didn't move: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Body.String() != "token=abc" {
		t.Errorf("moved file: %q", w.Body.String())
	}
	if _, err := e.files.Get(context.Background(), v1.AttachmentPurposeSubmission, s.Name, ref); err != attachments.ErrNotFound {
		t.Errorf("original file kept: %v", err)
	}

	reviewed := e.submit(t, alice, spec())
	if _, err := e.review(staff, reviewed.Name, v1.SubmissionReviewSpec{Decision: v1.ReviewDecisionRejected, Reason: "no"}); err != nil {
		t.Fatal(err)
	}
	if _, err := move(staff, reviewed.Name); !apierrors.IsConflict(err) {
		t.Errorf("reviewed submission moves: %v", err)
	}
}
