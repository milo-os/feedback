// Package submission implements storage for Submission: private feedback
// waiting for a staff decision.
//
// Who does what:
//   - Submitters create submissions in the user context and read their own,
//     without triage, the reviewer or the staff-edited public text.
//   - Staff list, read and delete every submission in the platform context,
//     and decide through the review subresource.
//   - feedback-controller writes status.triage; feedback-sync writes
//     status.postRef. Nobody else writes status.
package submission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/registry/vote"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const (
	maxTitle       = 200
	maxBody        = 20000
	maxAttachments = 5
	maxReason      = 2000
)

var (
	kinds     = []v1.SubmissionKind{v1.SubmissionKindIdea, v1.SubmissionKindBug, v1.SubmissionKindOther}
	decisions = []v1.ReviewDecision{v1.ReviewDecisionApproved, v1.ReviewDecisionMerged, v1.ReviewDecisionRejected}
)

// newName returns a random submission name. Names are always generated:
// they are unique across users, so a chosen name could reveal that someone
// else's submission exists.
func newName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "sub-" + hex.EncodeToString(b)
}

type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	ids identity.Identities
}

func (strategy) NamespaceScoped() bool { return false }

func (strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	s := obj.(*v1.Submission)
	s.Name, s.GenerateName = newName(), ""
	s.Status = v1.SubmissionStatus{Phase: v1.SubmissionPhasePendingReview}
}

// PrepareForUpdate keeps spec and status: the main resource only changes
// metadata.
func (strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	s, o := obj.(*v1.Submission), old.(*v1.Submission)
	s.Status = o.Status
}

func (strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return validateSpec(&obj.(*v1.Submission).Spec)
}

func (strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	if !equality.Semantic.DeepEqual(obj.(*v1.Submission).Spec, old.(*v1.Submission).Spec) {
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

func validateSpec(s *v1.SubmissionSpec) field.ErrorList {
	p := field.NewPath("spec")
	var errs field.ErrorList
	if !slices.Contains(kinds, s.Kind) {
		errs = append(errs, field.NotSupported(p.Child("kind"), s.Kind, kinds))
	}
	errs = append(errs, required(p.Child("title"), s.Title, maxTitle)...)
	errs = append(errs, required(p.Child("body"), s.Body, maxBody)...)
	if len(s.Attachments) > maxAttachments {
		errs = append(errs, field.TooMany(p.Child("attachments"), len(s.Attachments), maxAttachments))
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
	c, cp := s.Context, p.Child("context")
	if c.Client != "" && !slices.Contains(vote.Votable, c.Client) {
		errs = append(errs, field.NotSupported(cp.Child("client"), c.Client, vote.Votable))
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{{"pageURL", c.PageURL, 2048}, {"organization", c.Organization, 253}, {"project", c.Project, 253}, {"clientVersion", c.ClientVersion, 100}} {
		if len(f.value) > f.max {
			errs = append(errs, field.TooLong(cp.Child(f.name), "", f.max))
		}
	}
	if len(c.RequestIDs) > 20 {
		errs = append(errs, field.TooMany(cp.Child("requestIDs"), len(c.RequestIDs), 20))
	}
	return errs
}

func required(p *field.Path, v string, max int) field.ErrorList {
	switch {
	case v == "":
		return field.ErrorList{field.Required(p, "")}
	case len(v) > max:
		return field.ErrorList{field.TooLong(p, "", max)}
	}
	return nil
}

// statusStrategy lets feedback-controller write triage and feedback-sync
// write postRef. Fields a caller doesn't own are reset; anyone else is
// refused.
type statusStrategy struct {
	strategy
}

func (s statusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	n, o := obj.(*v1.Submission), old.(*v1.Submission)
	n.Spec = o.Spec
	n.Labels, n.Annotations = o.Labels, o.Annotations
	incoming := n.Status
	n.Status = *o.Status.DeepCopy()
	switch s.ids.Of(ctx) {
	case identity.Controller:
		n.Status.Triage = incoming.Triage
	case identity.Sync:
		n.Status.PostRef = incoming.PostRef
	}
}

func (s statusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	sp := field.NewPath("status")
	n, o := obj.(*v1.Submission), old.(*v1.Submission)
	switch s.ids.Of(ctx) {
	case identity.Other:
		return field.ErrorList{field.Forbidden(sp,
			"submission status is written by feedback-controller (triage), feedback-sync (postRef) and the review subresource")}
	case identity.Sync:
		if o.Status.PostRef != nil && !equality.Semantic.DeepEqual(n.Status.PostRef, o.Status.PostRef) {
			return field.ErrorList{field.Forbidden(sp.Child("postRef"), "is set once")}
		}
		if n.Status.PostRef != nil && n.Status.PostRef.Name == "" {
			return field.ErrorList{field.Required(sp.Child("postRef", "name"), "")}
		}
	case identity.Controller:
		if t := n.Status.Triage; t != nil {
			if t.FraudScore != nil && (*t.FraudScore < 0 || *t.FraudScore > 1) {
				return field.ErrorList{field.Invalid(sp.Child("triage", "fraudScore"), *t.FraudScore, "must be between 0 and 1")}
			}
			if k := t.SuggestedKind; k != "" && !slices.Contains(kinds, k) {
				return field.ErrorList{field.NotSupported(sp.Child("triage", "suggestedKind"), k, kinds)}
			}
		}
	}
	return nil
}

func (statusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		fieldpath.APIVersion(v1.SchemeGroupVersion.String()): fieldpath.NewSet(fieldpath.MakePathOrDie("spec")),
	}
}

// reviewStrategy applies a decision the review subresource already put in
// status; spec and metadata can't change.
type reviewStrategy struct {
	strategy
}

func (reviewStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	n, o := obj.(*v1.Submission), old.(*v1.Submission)
	n.Spec = o.Spec
	n.Labels, n.Annotations = o.Labels, o.Annotations
}

func (reviewStrategy) ValidateUpdate(context.Context, runtime.Object, runtime.Object) field.ErrorList {
	return nil
}

// redact is what a submitter sees of their submission's status: the phase,
// the post, and the decision with the rejection reason.
func redact(s *v1.Submission) {
	s.Status.Triage = nil
	if r := s.Status.Review; r != nil {
		s.Status.Review = &v1.SubmissionReviewOutcome{
			Decision: r.Decision, ReviewedAt: r.ReviewedAt, MergedInto: r.MergedInto, Reason: r.Reason,
		}
	}
}
