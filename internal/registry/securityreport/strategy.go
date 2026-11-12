// Package securityreport implements storage for SecurityReport and the
// SecurityAcknowledgement view.
//
// Who does what:
//   - Reporters create reports in the user context and read their own,
//     without status.internal. They can't change or withdraw them.
//   - The Security Team works every report in the platform context: it
//     writes status, and logs reports for people without a Datum account
//     (spec.reporter set), which belong to the platform. Milo IAM keeps
//     everyone else, feedback reviewers included, out.
//   - feedback-sync and feedback-controller never write reports.
//
// Reports are never embedded, indexed, synced or sent to the AI gateway:
// nothing here calls those paths.
package securityreport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var (
	phases = []v1.SecurityReportPhase{
		v1.SecurityReportPhaseReceived, v1.SecurityReportPhaseTriaged, v1.SecurityReportPhaseFixing,
		v1.SecurityReportPhaseFixed, v1.SecurityReportPhaseDisclosed,
		v1.SecurityReportPhaseNotApplicable, v1.SecurityReportPhaseDuplicate,
	}
	severities = []v1.Severity{v1.SeverityCritical, v1.SeverityHigh, v1.SeverityMedium, v1.SeverityLow, v1.SeverityInformational}
	rewards    = []v1.RewardKind{v1.RewardKindNone, v1.RewardKindThanks, v1.RewardKindSwag, v1.RewardKindCredits}
	clients    = []v1.Client{v1.ClientWebsite, v1.ClientPortal, v1.ClientDatumctl, v1.ClientPatch, v1.ClientEmail, v1.ClientGitHub}

	amountRE = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)
	cvssRE   = regexp.MustCompile(`^CVSS:[34]\.[01]/[A-Za-z:/]+$`)
	emailRE  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

func newName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "sec-" + hex.EncodeToString(b)
}

type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	ids identity.Identities
	now func() time.Time
}

func (strategy) NamespaceScoped() bool { return false }

// PrepareForCreate names the report (names are unique across reporters, so
// they are always generated) and starts its timeline.
func (s strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	r := obj.(*v1.SecurityReport)
	r.Name, r.GenerateName = newName(), ""
	r.Status = v1.SecurityReportStatus{
		Phase:    v1.SecurityReportPhaseReceived,
		Timeline: []v1.SecurityTimelineEntry{{Phase: v1.SecurityReportPhaseReceived, At: metav1.NewTime(s.now())}},
	}
}

func (strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	obj.(*v1.SecurityReport).Status = old.(*v1.SecurityReport).Status
}

func (strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateSpec(&obj.(*v1.SecurityReport).Spec)
}

func (strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	if !equality.Semantic.DeepEqual(obj.(*v1.SecurityReport).Spec, old.(*v1.SecurityReport).Spec) {
		return field.ErrorList{field.Forbidden(field.NewPath("spec"), "is immutable")}
	}
	return nil
}

func (strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (strategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (strategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (strategy) Canonicalize(runtime.Object)                   {}

func validateSpec(s *v1.SecurityReportSpec) field.ErrorList {
	p := field.NewPath("spec")
	var errs field.ErrorList
	errs = append(errs, text(p.Child("title"), s.Title, 200, true)...)
	errs = append(errs, text(p.Child("description"), s.Description, 20000, true)...)
	errs = append(errs, text(p.Child("affected"), s.Affected, 200, false)...)
	errs = append(errs, text(p.Child("stepsToReproduce"), s.StepsToReproduce, 20000, false)...)
	errs = append(errs, text(p.Child("impact"), s.Impact, 5000, false)...)
	if len(s.Attachments) > 5 {
		errs = append(errs, field.TooMany(p.Child("attachments"), len(s.Attachments), 5))
	}
	seen := map[string]bool{}
	for i, a := range s.Attachments {
		ap := p.Child("attachments").Index(i).Child("uploadRef")
		switch {
		case a.UploadRef == "":
			errs = append(errs, field.Required(ap, ""))
		case seen[a.UploadRef]:
			errs = append(errs, field.Duplicate(ap, a.UploadRef))
		}
		seen[a.UploadRef] = true
	}
	if c := s.Credit; c != nil {
		cp := p.Child("credit")
		errs = append(errs, text(cp.Child("name"), c.Name, 100, c.Public)...)
		if c.URL != "" {
			if u, err := url.Parse(c.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(c.URL) > 2048 {
				errs = append(errs, field.Invalid(cp.Child("url"), c.URL, "must be an http(s) URL"))
			}
		}
	}
	if r := s.Reporter; r != nil {
		rp := p.Child("reporter")
		if r.Email == "" && r.GitHubLogin == "" {
			errs = append(errs, field.Required(rp, "email or githubLogin"))
		}
		if r.Email != "" && !emailRE.MatchString(r.Email) {
			errs = append(errs, field.Invalid(rp.Child("email"), r.Email, "must be an email address"))
		}
	}
	if c := s.Context.Client; c != "" && !slices.Contains(clients, c) {
		errs = append(errs, field.NotSupported(p.Child("context", "client"), c, clients))
	}
	return errs
}

func text(p *field.Path, v string, max int, required bool) field.ErrorList {
	switch {
	case v == "" && required:
		return field.ErrorList{field.Required(p, "")}
	case len(v) > max:
		return field.ErrorList{field.TooLong(p, "", max)}
	}
	return nil
}

// statusStrategy is the Security Team's status update. A phase change
// without a timeline entry for it gets one, so the reporter always sees it.
type statusStrategy struct {
	strategy
}

func (s statusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n, o := obj.(*v1.SecurityReport), old.(*v1.SecurityReport)
	n.Spec = o.Spec
	n.Labels, n.Annotations = o.Labels, o.Annotations
	st := &n.Status
	if st.Phase != o.Status.Phase {
		if l := len(st.Timeline); l == 0 || st.Timeline[l-1].Phase != st.Phase {
			st.Timeline = append(st.Timeline, v1.SecurityTimelineEntry{Phase: st.Phase, At: metav1.NewTime(s.now())})
		}
	}
}

func (s statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	sp := field.NewPath("status")
	if s.ids.Of(ctx) != identity.Other {
		return field.ErrorList{field.Forbidden(sp, "security report status is written by the Security Team")}
	}
	n, o := obj.(*v1.SecurityReport), old.(*v1.SecurityReport)
	st := n.Status
	var errs field.ErrorList
	if !slices.Contains(phases, st.Phase) {
		errs = append(errs, field.NotSupported(sp.Child("phase"), st.Phase, phases))
	}
	if st.Severity != "" && !slices.Contains(severities, st.Severity) {
		errs = append(errs, field.NotSupported(sp.Child("severity"), st.Severity, severities))
	}
	if st.CVSSVector != "" && !cvssRE.MatchString(st.CVSSVector) {
		errs = append(errs, field.Invalid(sp.Child("cvssVector"), st.CVSSVector, "must be a CVSS 3.x or 4.0 vector"))
	}
	switch st.Phase {
	case v1.SecurityReportPhaseNotApplicable, v1.SecurityReportPhaseDuplicate:
		errs = append(errs, text(sp.Child("reason"), st.Reason, 2000, true)...)
	}
	if st.Phase == v1.SecurityReportPhaseDuplicate {
		dp := sp.Child("internal", "duplicateOf")
		switch {
		case st.Internal == nil || st.Internal.DuplicateOf == "":
			errs = append(errs, field.Required(dp, "the original report"))
		case st.Internal.DuplicateOf == n.Name:
			errs = append(errs, field.Invalid(dp, n.Name, "a report can't duplicate itself"))
		}
	}
	// The timeline is the reporter's record: entries can be added, not
	// changed or removed.
	tp := sp.Child("timeline")
	if len(st.Timeline) < len(o.Status.Timeline) || !equality.Semantic.DeepEqual(st.Timeline[:len(o.Status.Timeline)], o.Status.Timeline) {
		errs = append(errs, field.Forbidden(tp, "entries can be added but not changed or removed"))
	}
	for i, e := range st.Timeline {
		if !slices.Contains(phases, e.Phase) {
			errs = append(errs, field.NotSupported(tp.Index(i).Child("phase"), e.Phase, phases))
		}
		errs = append(errs, text(tp.Index(i).Child("note"), e.Note, 2000, false)...)
	}
	if r := st.Reward; r != nil {
		rp := sp.Child("reward")
		if !slices.Contains(rewards, r.Kind) {
			errs = append(errs, field.NotSupported(rp.Child("kind"), r.Kind, rewards))
		}
		if r.Kind == v1.RewardKindCredits && !amountRE.MatchString(r.Amount) {
			errs = append(errs, field.Invalid(rp.Child("amount"), r.Amount, "credits need an amount like 250.00"))
		}
	}
	if a := st.Advisory; a != nil && a.URL != "" {
		if u, err := url.Parse(a.URL); err != nil || u.Scheme != "https" {
			errs = append(errs, field.Invalid(sp.Child("advisory", "url"), a.URL, "must be an https URL"))
		}
	}
	return errs
}

func (statusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		fieldpath.APIVersion(v1.SchemeGroupVersion.String()): fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}

// redact is what a reporter sees: everything but the internal notes.
func redact(r *v1.SecurityReport) {
	r.Status.Internal = nil
}
