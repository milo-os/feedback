package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var (
	amountRE = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)
	cvssRE   = regexp.MustCompile(`^CVSS:[34]\.[01]/[A-Za-z:/]+$`)
	emailRE  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// +kubebuilder:webhook:path=/validate-securityreport,mutating=false,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=securityreports;securityreports/status,verbs=create;update,versions=v1alpha1,name=securityreport.feedback.miloapis.com,admissionReviewVersions=v1

// SecurityReportCheck validates what the CRD schema can't: credit links,
// external reporters' contact details, and the Security Team's status
// updates. The timeline is the reporter's record, so entries can be added but
// not changed or removed.
type SecurityReportCheck struct{}

var _ admission.Handler = SecurityReportCheck{}

// Handle implements admission.Handler.
func (SecurityReportCheck) Handle(_ context.Context, req admission.Request) admission.Response {
	r := &v1.SecurityReport{}
	if err := json.Unmarshal(req.Object.Raw, r); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errs field.ErrorList
	switch {
	case req.Operation == admissionv1.Create:
		errs = validateReportSpec(&r.Spec)
	case req.SubResource == "status":
		old := &v1.SecurityReport{}
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		errs = validateReportStatus(&r.Status, &old.Status)
	}
	if len(errs) == 0 {
		return admission.Allowed("")
	}
	return admission.Denied(apierrors.NewInvalid(v1.Kind("SecurityReport"), r.Name, errs).Error())
}

func validateReportSpec(s *v1.SecurityReportSpec) field.ErrorList {
	p := field.NewPath("spec")
	var errs field.ErrorList
	if c := s.Credit; c != nil {
		cp := p.Child("credit")
		if c.Public && c.Name == "" {
			errs = append(errs, field.Required(cp.Child("name"), "public credit needs a name"))
		}
		if len(c.Name) > 100 {
			errs = append(errs, field.TooLong(cp.Child("name"), "", 100))
		}
		if c.URL != "" {
			if u, err := url.Parse(c.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(c.URL) > 2048 {
				errs = append(errs, field.Invalid(cp.Child("url"), c.URL, "must be an http(s) URL"))
			}
		}
	}
	rp := p.Child("reporter")
	switch r := s.Reporter; {
	case s.UserRef == nil && r == nil:
		errs = append(errs, field.Required(rp, "reports logged for someone without an account need their email or GitHub username"))
	case r != nil && r.Email == "" && r.GitHubLogin == "":
		errs = append(errs, field.Required(rp, "email or githubLogin"))
	case r != nil && r.Email != "" && !emailRE.MatchString(r.Email):
		errs = append(errs, field.Invalid(rp.Child("email"), r.Email, "must be an email address"))
	}
	return errs
}

func validateReportStatus(st, old *v1.SecurityReportStatus) field.ErrorList {
	sp := field.NewPath("status")
	var errs field.ErrorList
	if st.CVSSVector != "" && !cvssRE.MatchString(st.CVSSVector) {
		errs = append(errs, field.Invalid(sp.Child("cvssVector"), st.CVSSVector, "must be a CVSS 3.x or 4.0 vector"))
	}
	switch st.Phase {
	case v1.SecurityReportPhaseNotApplicable, v1.SecurityReportPhaseDuplicate:
		if st.Reason == "" {
			errs = append(errs, field.Required(sp.Child("reason"), "the reporter is told why"))
		}
	}
	if len(st.Reason) > 2000 {
		errs = append(errs, field.TooLong(sp.Child("reason"), "", 2000))
	}
	tp := sp.Child("timeline")
	if len(st.Timeline) < len(old.Timeline) || !equality.Semantic.DeepEqual(st.Timeline[:len(old.Timeline)], old.Timeline) {
		errs = append(errs, field.Forbidden(tp, "entries can be added but not changed or removed"))
	}
	if st.Phase != old.Phase {
		if l := len(st.Timeline); l == 0 || st.Timeline[l-1].Phase != st.Phase {
			errs = append(errs, field.Required(tp, "a phase change needs a timeline entry for the new phase"))
		}
	}
	for i, e := range st.Timeline {
		if len(e.Note) > 2000 {
			errs = append(errs, field.TooLong(tp.Index(i).Child("note"), "", 2000))
		}
	}
	if r := st.Reward; r != nil && r.Kind == v1.RewardKindCredits && !amountRE.MatchString(r.Amount) {
		errs = append(errs, field.Invalid(sp.Child("reward", "amount"), r.Amount, "credits need an amount like 250.00"))
	}
	if a := st.Advisory; a != nil && a.URL != "" {
		if u, err := url.Parse(a.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, field.Invalid(sp.Child("advisory", "url"), a.URL, "must be an https URL"))
		}
	}
	return errs
}

// +kubebuilder:webhook:path=/mutate-securityreport-status,mutating=true,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=securityreports/status,verbs=update,versions=v1alpha1,name=securitytimeline.feedback.miloapis.com,admissionReviewVersions=v1

// SecurityTimeline adds a timeline entry when a status update changes the
// phase without one, so the reporter always sees each phase.
type SecurityTimeline struct {
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

var _ admission.Handler = SecurityTimeline{}

// Handle implements admission.Handler.
func (t SecurityTimeline) Handle(_ context.Context, req admission.Request) admission.Response {
	r, old := &v1.SecurityReport{}, &v1.SecurityReport{}
	if err := json.Unmarshal(req.Object.Raw, r); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	st := &r.Status
	if st.Phase == old.Status.Phase || st.Phase == "" {
		return admission.Allowed("")
	}
	if l := len(st.Timeline); l > 0 && st.Timeline[l-1].Phase == st.Phase {
		return admission.Allowed("")
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	st.Timeline = append(st.Timeline, v1.SecurityTimelineEntry{Phase: st.Phase, At: metav1.NewTime(now().UTC().Truncate(time.Second))})
	raw, err := json.Marshal(r)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
