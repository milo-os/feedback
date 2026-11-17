package webhook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

func inUserContext(id string) authenticationv1.UserInfo {
	return authenticationv1.UserInfo{Username: "ada@example.com", Extra: map[string]authenticationv1.ExtraValue{
		identity.ParentKindExtraKey: {"User"},
		identity.ParentNameExtraKey: {id},
	}}
}

func request(t *testing.T, op admissionv1.Operation, resource string, user authenticationv1.UserInfo, obj, old runtime.Object) admission.Request {
	t.Helper()
	raw := func(o runtime.Object) runtime.RawExtension {
		if o == nil {
			return runtime.RawExtension{}
		}
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: b}
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op,
		Resource:  metav1.GroupVersionResource{Group: v1.GroupName, Version: "v1alpha1", Resource: resource},
		UserInfo:  user,
		Object:    raw(obj),
		OldObject: raw(old),
	}}
}

// patched applies resp's patches to the request object.
func patched(t *testing.T, req admission.Request, resp admission.Response, into any) {
	t.Helper()
	ops, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatal(err)
	}
	p, err := jsonpatch.DecodePatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Apply(req.Object.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, into); err != nil {
		t.Fatal(err)
	}
}

func vote(owner string) *v1.Vote {
	return &v1.Vote{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1.SchemeGroupVersion.String(), Kind: "Vote"},
		ObjectMeta: metav1.ObjectMeta{Name: v1.OwnedName("p", "u1")},
		Spec:       v1.VoteSpec{PostRef: v1.PostReference{Name: "p"}, UserRef: v1.UserReference{Name: owner}},
	}
}

func TestOwnerSetsTheCallerInTheUserContext(t *testing.T) {
	for _, sent := range []string{"", "someone-else"} {
		req := request(t, admissionv1.Create, "votes", inUserContext("u1"), vote(sent), nil)
		resp := Owner{}.Handle(context.Background(), req)
		if !resp.Allowed {
			t.Fatalf("sent %q: denied: %v", sent, resp.Result)
		}
		var got v1.Vote
		patched(t, req, resp, &got)
		if got.Spec.UserRef.Name != "u1" {
			t.Errorf("sent %q: owner = %q, want u1", sent, got.Spec.UserRef.Name)
		}
	}

	req := request(t, admissionv1.Create, "votes", inUserContext("u1"), vote("u1"), nil)
	if resp := (Owner{}).Handle(context.Background(), req); !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("already the caller: allowed %v, %d patches", resp.Allowed, len(resp.Patches))
	}
}

func TestOwnerOutsideTheUserContext(t *testing.T) {
	staff := authenticationv1.UserInfo{Username: "staff@datum.net"}
	operator := authenticationv1.UserInfo{Username: identity.OperatorUser}
	report := func(owner string) *v1.SecurityReport {
		r := &v1.SecurityReport{Spec: v1.SecurityReportSpec{Title: "t", Description: "d"}}
		if owner != "" {
			r.Spec.UserRef = &v1.UserReference{Name: owner}
		}
		return r
	}
	for _, tc := range []struct {
		name     string
		resource string
		user     authenticationv1.UserInfo
		obj      runtime.Object
		allowed  bool
	}{
		{"staff can't vote for someone", "votes", staff, vote("u1"), false},
		{"staff can't vote", "votes", staff, vote(""), false},
		{"operator votes for a user", "votes", operator, vote("u1"), true},
		{"operator must name the owner", "votes", operator, vote(""), false},
		{"staff log an external report", "securityreports", staff, report(""), true},
		{"staff can't own a report for someone", "securityreports", staff, report("u1"), false},
		{"operator moves a report for a user", "securityreports", operator, report("u1"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := Owner{}.Handle(context.Background(), request(t, admissionv1.Create, tc.resource, tc.user, tc.obj, nil))
			if resp.Allowed != tc.allowed {
				t.Errorf("allowed = %v, want %v (%v)", resp.Allowed, tc.allowed, resp.Result)
			}
		})
	}
}

func TestOwnerIgnoresUpdates(t *testing.T) {
	req := request(t, admissionv1.Update, "votes", authenticationv1.UserInfo{Username: "x"}, vote("u2"), vote("u1"))
	if resp := (Owner{}).Handle(context.Background(), req); !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("update: allowed %v, %d patches", resp.Allowed, len(resp.Patches))
	}
}

func TestReviewerRecordsWhoDecided(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	r := Reviewer{Now: func() time.Time { return now }}
	staff := authenticationv1.UserInfo{Username: "staff@datum.net"}
	review := func(decision v1.ReviewDecision, reviewer string) *v1.SubmissionReview {
		return &v1.SubmissionReview{
			ObjectMeta: metav1.ObjectMeta{Name: "sub-1"},
			Spec:       v1.SubmissionReviewSpec{Decision: decision, Reason: "x", Reviewer: reviewer},
		}
	}

	req := request(t, admissionv1.Update, "submissionreviews", staff, review(v1.ReviewDecisionRejected, "someone-else"), review("", ""))
	resp := r.Handle(context.Background(), req)
	var got v1.SubmissionReview
	patched(t, req, resp, &got)
	if got.Spec.Reviewer != "staff@datum.net" || got.Spec.ReviewedAt == nil || !got.Spec.ReviewedAt.Time.Equal(now) {
		t.Errorf("decided: reviewer %q at %v", got.Spec.Reviewer, got.Spec.ReviewedAt)
	}

	req = request(t, admissionv1.Update, "submissionreviews", staff, review("", "someone-else"), review("", ""))
	resp = r.Handle(context.Background(), req)
	got = v1.SubmissionReview{}
	patched(t, req, resp, &got)
	if got.Spec.Reviewer != "" {
		t.Errorf("undecided: reviewer %q kept", got.Spec.Reviewer)
	}

	// Already decided: left to the CRD's immutability rule.
	req = request(t, admissionv1.Update, "submissionreviews", staff, review(v1.ReviewDecisionRejected, "a"), review(v1.ReviewDecisionRejected, "a"))
	if resp := r.Handle(context.Background(), req); !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("decided earlier: allowed %v, %d patches", resp.Allowed, len(resp.Patches))
	}
}
