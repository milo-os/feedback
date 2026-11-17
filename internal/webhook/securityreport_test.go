package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func TestSecurityReportSpec(t *testing.T) {
	staff := authenticationv1.UserInfo{Username: "sec@datum.net"}
	report := func(f func(*v1.SecurityReportSpec)) *v1.SecurityReport {
		r := &v1.SecurityReport{Spec: v1.SecurityReportSpec{UserRef: &v1.UserReference{Name: "u1"}, Title: "t", Description: "d"}}
		f(&r.Spec)
		return r
	}
	for _, tc := range []struct {
		name   string
		spec   func(*v1.SecurityReportSpec)
		denied string
	}{
		{"a user's report", func(*v1.SecurityReportSpec) {}, ""},
		{"external, with an email", func(s *v1.SecurityReportSpec) {
			s.UserRef, s.Reporter = nil, &v1.ExternalReporter{Email: "r@example.com"}
		}, ""},
		{"external, no contact", func(s *v1.SecurityReportSpec) { s.UserRef = nil }, "spec.reporter"},
		{"bad email", func(s *v1.SecurityReportSpec) { s.Reporter = &v1.ExternalReporter{Email: "nope"} }, "email address"},
		{"public credit without a name", func(s *v1.SecurityReportSpec) { s.Credit = &v1.SecurityCredit{Public: true} }, "needs a name"},
		{"credit link isn't http", func(s *v1.SecurityReportSpec) {
			s.Credit = &v1.SecurityCredit{Name: "Ada", URL: "javascript:alert(1)"}
		}, "http(s) URL"},
	} {
		resp := SecurityReportCheck{}.Handle(context.Background(), request(t, admissionv1.Create, "securityreports", staff, report(tc.spec), nil))
		if tc.denied == "" && !resp.Allowed {
			t.Errorf("%s: denied: %v", tc.name, resp.Result.Message)
		}
		if tc.denied != "" && (resp.Allowed || !strings.Contains(resp.Result.Message, tc.denied)) {
			t.Errorf("%s: allowed %v, %q; want %q", tc.name, resp.Allowed, resp.Result.Message, tc.denied)
		}
	}
}

func TestSecurityReportStatus(t *testing.T) {
	staff := authenticationv1.UserInfo{Username: "sec@datum.net"}
	at := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	received := v1.SecurityTimelineEntry{Phase: v1.SecurityReportPhaseReceived, At: at}
	triaged := v1.SecurityTimelineEntry{Phase: v1.SecurityReportPhaseTriaged, At: at, Note: "Confirmed"}
	old := &v1.SecurityReport{Status: v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseReceived, Timeline: []v1.SecurityTimelineEntry{received}}}
	with := func(st v1.SecurityReportStatus) *v1.SecurityReport { return &v1.SecurityReport{Status: st} }
	check := func(obj *v1.SecurityReport) (bool, string) {
		req := request(t, admissionv1.Update, "securityreports", staff, obj, old)
		req.SubResource = "status"
		resp := SecurityReportCheck{}.Handle(context.Background(), req)
		return resp.Allowed, resp.Result.Message
	}
	for _, tc := range []struct {
		name   string
		st     v1.SecurityReportStatus
		denied string
	}{
		{"triaged with an entry", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseTriaged, Timeline: []v1.SecurityTimelineEntry{received, triaged}}, ""},
		{"phase change without an entry", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseTriaged, Timeline: []v1.SecurityTimelineEntry{received}}, "timeline entry"},
		{"rewriting history", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseReceived, Timeline: []v1.SecurityTimelineEntry{triaged}}, "added but not changed"},
		{"not applicable without a reason", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseNotApplicable,
			Timeline: []v1.SecurityTimelineEntry{received, {Phase: v1.SecurityReportPhaseNotApplicable, At: at}}}, "status.reason"},
		{"credits without an amount", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseReceived, Timeline: []v1.SecurityTimelineEntry{received},
			Reward: &v1.SecurityReward{Kind: v1.RewardKindCredits}}, "amount"},
		{"advisory over http", v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseReceived, Timeline: []v1.SecurityTimelineEntry{received},
			Advisory: &v1.SecurityAdvisory{URL: "http://example.com"}}, "https"},
	} {
		ok, msg := check(with(tc.st))
		if tc.denied == "" && !ok {
			t.Errorf("%s: denied: %s", tc.name, msg)
		}
		if tc.denied != "" && (ok || !strings.Contains(msg, tc.denied)) {
			t.Errorf("%s: allowed %v, %q; want %q", tc.name, ok, msg, tc.denied)
		}
	}
}

func TestSecurityTimeline(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	tl := SecurityTimeline{Now: func() time.Time { return now }}
	staff := authenticationv1.UserInfo{Username: "sec@datum.net"}
	received := v1.SecurityTimelineEntry{Phase: v1.SecurityReportPhaseReceived, At: metav1.NewTime(now.Add(-time.Hour))}
	old := &v1.SecurityReport{Status: v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseReceived, Timeline: []v1.SecurityTimelineEntry{received}}}
	obj := &v1.SecurityReport{Status: v1.SecurityReportStatus{Phase: v1.SecurityReportPhaseFixing, Timeline: []v1.SecurityTimelineEntry{received}}}
	req := request(t, admissionv1.Update, "securityreports", staff, obj, old)
	resp := tl.Handle(context.Background(), req)
	var got v1.SecurityReport
	patched(t, req, resp, &got)
	if l := got.Status.Timeline; len(l) != 2 || l[1].Phase != v1.SecurityReportPhaseFixing || !l[1].At.Time.Equal(now) {
		t.Errorf("timeline %+v", l)
	}
}
