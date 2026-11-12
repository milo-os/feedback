package securityreport

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
	"go.miloapis.com/feedback/internal/registry/attachmentupload"
	"go.miloapis.com/feedback/internal/storage/pg"
	"go.miloapis.com/feedback/internal/storage/pg/pgtest"
	"go.miloapis.com/feedback/pkg/apis/feedback/install"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var (
	ids     = identity.Identities{Sync: []string{"sync"}, Controller: []string{"controller"}}
	secTeam = pgtest.AsPlatform("sec@datum.net")
	syncCtx = pgtest.AsPlatform("sync")
	alice   = pgtest.AsUser("alice")
	bob     = pgtest.AsUser("bob")
)

type env struct {
	db      *pg.DB
	r       *REST
	acks    *AcknowledgementREST
	files   *attachments.Store
	uploads *attachmentupload.REST
}

func setup(t *testing.T) env {
	t.Helper()
	db := pgtest.Require(t, "security_reports")
	scheme := runtime.NewScheme()
	install.Install(scheme)
	files, err := attachments.Open(context.Background(), "mem://")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	r, err := NewREST(db, scheme, Options{Identities: ids, Files: files, Limit: 3, Window: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return env{db, r, NewAcknowledgementREST(db), files, attachmentupload.NewREST(files, nil)}
}

func spec() v1.SecurityReportSpec {
	return v1.SecurityReportSpec{Title: "Viewer reads connector credentials", Description: "Steps...",
		Credit: &v1.SecurityCredit{Public: true, Name: "Ada L.", URL: "https://example.com"}}
}

func (e env) report(t *testing.T, ctx context.Context, s v1.SecurityReportSpec) *v1.SecurityReport {
	t.Helper()
	obj, err := e.r.Create(ctx, &v1.SecurityReport{Spec: s}, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return obj.(*v1.SecurityReport)
}

func (e env) get(t *testing.T, ctx context.Context, name string) *v1.SecurityReport {
	t.Helper()
	obj, err := e.r.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return obj.(*v1.SecurityReport)
}

func (e env) setStatus(ctx context.Context, name string, f func(*v1.SecurityReportStatus)) (*v1.SecurityReport, error) {
	obj, _, err := e.r.Status.Update(ctx, name, rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
		r := old.DeepCopyObject().(*v1.SecurityReport)
		f(&r.Status)
		return r, nil
	}), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return obj.(*v1.SecurityReport), nil
}

func TestReporterFlow(t *testing.T) {
	e := setup(t)
	in := &v1.SecurityReport{ObjectMeta: metav1.ObjectMeta{Name: "chosen"}, Spec: spec(),
		Status: v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseFixed, Internal: &v1.SecurityReportInternal{Notes: "x"}}}
	obj, err := e.r.Create(alice, in, nil, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := obj.(*v1.SecurityReport)
	if !strings.HasPrefix(r.Name, "sec-") || r.Status.Phase != v1.SecurityReportPhaseReceived ||
		len(r.Status.Timeline) != 1 || r.Status.Internal != nil {
		t.Errorf("created: %s %+v", r.Name, r.Status)
	}
	if _, err := e.r.Get(bob, r.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("bob reads alice's report: %v", err)
	}
	if got := e.get(t, secTeam, r.Name); got.Annotations[pg.OwnerAnnotation] != "alice" {
		t.Errorf("owner: %v", got.Annotations)
	}

	withReporter := spec()
	withReporter.Reporter = &v1.ExternalReporter{Email: "x@example.com"}
	unverified := request.WithUser(request.WithNamespace(context.Background(), ""), &user.DefaultInfo{Name: "carol", Extra: map[string][]string{
		pg.ParentKindExtraKey: {"User"}, pg.ParentNameExtraKey: {"carol"}, "iam.miloapis.com/emailverified": {"false"}}})
	badURL := spec()
	badURL.Credit.URL = "javascript:alert(1)"
	noName := spec()
	noName.Credit.Name = ""
	for name, c := range map[string]struct {
		ctx  context.Context
		spec v1.SecurityReportSpec
		want func(error) bool
	}{
		"reporter from user":  {alice, withReporter, apierrors.IsInvalid},
		"unverified":          {unverified, spec(), apierrors.IsForbidden},
		"credit url":          {alice, badURL, apierrors.IsInvalid},
		"public without name": {alice, noName, apierrors.IsInvalid},
		"org context":         {pgtest.AsContext("Organization", "acme"), spec(), apierrors.IsForbidden},
	} {
		if _, err := e.r.Create(c.ctx, &v1.SecurityReport{Spec: c.spec}, nil, &metav1.CreateOptions{}); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}

	r.Labels = map[string]string{"a": "b"}
	if _, _, err := e.r.Update(alice, r.Name, rest.DefaultUpdatedObjectInfo(r), nil, nil, false, &metav1.UpdateOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("reporter update: %v", err)
	}
	if _, _, err := e.r.Delete(alice, r.Name, nil, &metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("reporter delete: %v", err)
	}
	if _, err := e.setStatus(alice, r.Name, func(st *v1.SecurityReportStatus) { st.Phase = v1.SecurityReportPhaseFixed }); err == nil {
		t.Error("reporter status write allowed")
	}

	// Rate limit: 3 per window, the first already counts.
	e.report(t, alice, spec())
	e.report(t, alice, spec())
	if _, err := e.r.Create(alice, &v1.SecurityReport{Spec: spec()}, nil, &metav1.CreateOptions{}); !apierrors.IsTooManyRequests(err) {
		t.Errorf("4th report: %v", err)
	}
}

func TestSecurityTeamStatus(t *testing.T) {
	e := setup(t)
	r := e.report(t, alice, spec())

	out, err := e.setStatus(secTeam, r.Name, func(st *v1.SecurityReportStatus) {
		st.Phase = v1.SecurityReportPhaseTriaged
		st.Severity = v1.SeverityHigh
		st.CVSSVector = "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N"
		st.Internal = &v1.SecurityReportInternal{Assignee: "sec@datum.net", Notes: "repro'd"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if tl := out.Status.Timeline; len(tl) != 2 || tl[1].Phase != v1.SecurityReportPhaseTriaged {
		t.Errorf("timeline not appended: %+v", tl)
	}
	mine := e.get(t, alice, r.Name)
	if mine.Status.Internal != nil || mine.Status.Severity != v1.SeverityHigh || len(mine.Status.Timeline) != 2 {
		t.Errorf("reporter's view: %+v", mine.Status)
	}

	// A note the team adds with the phase change is kept, not doubled.
	out, err = e.setStatus(secTeam, r.Name, func(st *v1.SecurityReportStatus) {
		st.Phase = v1.SecurityReportPhaseFixing
		st.Timeline = append(st.Timeline, v1.SecurityTimelineEntry{Phase: v1.SecurityReportPhaseFixing, At: metav1.Now(), Note: "Fix in review"})
	})
	if err != nil || len(out.Status.Timeline) != 3 || out.Status.Timeline[2].Note != "Fix in review" {
		t.Fatalf("timeline with note: %+v %v", out, err)
	}

	for name, f := range map[string]func(*v1.SecurityReportStatus){
		"rewrite timeline": func(st *v1.SecurityReportStatus) { st.Timeline[0].Note = "edited" },
		"drop timeline":    func(st *v1.SecurityReportStatus) { st.Timeline = st.Timeline[:1] },
		"bad severity":     func(st *v1.SecurityReportStatus) { st.Severity = "spicy" },
		"n/a no reason":    func(st *v1.SecurityReportStatus) { st.Phase = v1.SecurityReportPhaseNotApplicable },
		"dup no original":  func(st *v1.SecurityReportStatus) { st.Phase, st.Reason = v1.SecurityReportPhaseDuplicate, "dup" },
		"credits":          func(st *v1.SecurityReportStatus) { st.Reward = &v1.SecurityReward{Kind: v1.RewardKindCredits} },
		"advisory http":    func(st *v1.SecurityReportStatus) { st.Advisory = &v1.SecurityAdvisory{URL: "http://x"} },
	} {
		if _, err := e.setStatus(secTeam, r.Name, f); !apierrors.IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.setStatus(syncCtx, r.Name, func(st *v1.SecurityReportStatus) { st.Severity = v1.SeverityLow }); !apierrors.IsInvalid(err) {
		t.Errorf("sync writes status: %v", err)
	}
	if _, err := e.setStatus(secTeam, r.Name, func(st *v1.SecurityReportStatus) {
		st.Reward = &v1.SecurityReward{Kind: v1.RewardKindCredits, Amount: "250.00", CreditGrantRef: &v1.CreditGrantReference{Name: "g"}}
	}); err != nil {
		t.Errorf("credits reward: %v", err)
	}
}

func TestExternalReports(t *testing.T) {
	e := setup(t)
	s := spec()
	s.Reporter = &v1.ExternalReporter{Email: "researcher@example.com"}
	s.Context.Client = v1.ClientEmail
	r := e.report(t, secTeam, s)
	got := e.get(t, secTeam, r.Name)
	if got.Annotations[pg.OwnerAnnotation] != "" || got.Spec.Reporter.Email != "researcher@example.com" {
		t.Errorf("external report: %+v", got)
	}
	var scope string
	_ = e.db.Pool.QueryRow(context.Background(), "SELECT scope_type FROM security_reports WHERE name = $1", r.Name).Scan(&scope)
	if scope != pg.ScopePlatform {
		t.Errorf("scope %q", scope)
	}
	if _, err := e.r.Create(secTeam, &v1.SecurityReport{Spec: spec()}, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("platform report without reporter: %v", err)
	}
	if _, err := e.r.Create(syncCtx, &v1.SecurityReport{Spec: s}, nil, &metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("sync logs a report: %v", err)
	}
	list, err := e.r.List(alice, &metainternalversion.ListOptions{})
	if err != nil || len(list.(*v1.SecurityReportList).Items) != 0 {
		t.Errorf("alice sees external reports: %v %v", list, err)
	}
}

func TestAcknowledgements(t *testing.T) {
	e := setup(t)
	mk := func(ctx context.Context, public bool, phases ...v1.SecurityReportPhase) string {
		s := spec()
		s.Credit.Public = public
		r := e.report(t, ctx, s)
		for _, p := range phases {
			if _, err := e.setStatus(secTeam, r.Name, func(st *v1.SecurityReportStatus) {
				st.Phase, st.Severity = p, v1.SeverityMedium
				st.Internal = &v1.SecurityReportInternal{Notes: "secret"}
			}); err != nil {
				t.Fatal(err)
			}
		}
		return r.Name
	}
	fixed := mk(alice, true, v1.SecurityReportPhaseTriaged, v1.SecurityReportPhaseFixed)
	disclosed := mk(bob, true, v1.SecurityReportPhaseFixed, v1.SecurityReportPhaseDisclosed)
	private := mk(pgtest.AsUser("carol"), false, v1.SecurityReportPhaseFixed)
	open := mk(pgtest.AsUser("dave"), true, v1.SecurityReportPhaseTriaged)
	// The month comes from the first Fixed entry.
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE security_reports
		SET object = jsonb_set(object, '{status,timeline,2,at}', '"2026-03-04T00:00:00Z"') WHERE name = $1`, fixed); err != nil {
		t.Fatal(err)
	}

	obj, err := e.acks.List(pgtest.AsPlatform("datum-net"), &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items := obj.(*v1.SecurityAcknowledgementList).Items
	if len(items) != 2 || items[0].Name != disclosed || items[1].Name != fixed {
		t.Fatalf("acknowledgements: %+v", items)
	}
	a := items[1].Status
	if a.Name != "Ada L." || a.URL != "https://example.com" || a.Month != "2026-03" || a.Severity != v1.SeverityMedium {
		t.Errorf("acknowledgement: %+v", a)
	}
	if _, err := e.acks.Get(alice, private, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("private credit: %v", err)
	}
	if _, err := e.acks.Get(alice, open, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("unfixed report: %v", err)
	}
	if _, err := e.acks.Get(alice, fixed, &metav1.GetOptions{}); err != nil {
		t.Errorf("get: %v", err)
	}
}

func TestAttachments(t *testing.T) {
	e := setup(t)
	upload := func(ctx context.Context, p v1.AttachmentPurpose) string {
		t.Helper()
		obj, err := e.uploads.Create(ctx, &v1.AttachmentUpload{Spec: v1.AttachmentUploadSpec{
			Purpose: p, Name: "poc.txt", ContentType: "text/plain", Data: []byte("curl ...")}}, nil, &metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return obj.(*v1.AttachmentUpload).Status.UploadRef
	}
	wrongPurpose := upload(alice, v1.AttachmentPurposeSubmission)
	ref := upload(alice, v1.AttachmentPurposeSecurityReport)

	s := spec()
	s.Attachments = []v1.AttachmentReference{{UploadRef: wrongPurpose}}
	if _, err := e.r.Create(alice, &v1.SecurityReport{Spec: s}, nil, &metav1.CreateOptions{}); !apierrors.IsInvalid(err) {
		t.Errorf("submission upload on a report: %v", err)
	}
	s.Attachments = []v1.AttachmentReference{{UploadRef: ref}}
	r := e.report(t, alice, s)
	if a := r.Spec.Attachments[0]; a.Name != "poc.txt" || a.ContentType != "text/plain" {
		t.Errorf("attachment: %+v", a)
	}
	for _, c := range []struct {
		ctx  context.Context
		want bool
	}{{alice, true}, {secTeam, true}, {bob, false}} {
		h, err := e.r.Attachment.Connect(c.ctx, r.Name, &v1.AttachmentOptions{Ref: ref}, nil)
		if (err == nil) != c.want {
			t.Errorf("download: %v", err)
			continue
		}
		if err == nil {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			if w.Body.String() != "curl ..." {
				t.Errorf("body %q", w.Body.String())
			}
		}
	}
}
