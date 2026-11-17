package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-submissionreview,mutating=true,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=submissionreviews,verbs=create;update,versions=v1alpha1,name=reviewer.feedback.miloapis.com,admissionReviewVersions=v1

// Reviewer records who decided on a submission. When a request sets
// spec.decision, it sets spec.reviewer and spec.reviewedAt from the request;
// while there is no decision it clears them, so a client can't name someone
// else. After that the CRD keeps the whole spec immutable.
type Reviewer struct {
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

var _ admission.Handler = Reviewer{}

// Handle implements admission.Handler.
func (r Reviewer) Handle(_ context.Context, req admission.Request) admission.Response {
	review := &v1.SubmissionReview{}
	if err := json.Unmarshal(req.Object.Raw, review); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var oldDecision v1.ReviewDecision
	if req.Operation == admissionv1.Update {
		old := &v1.SubmissionReview{}
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		oldDecision = old.Spec.Decision
	}

	s := &review.Spec
	switch {
	case oldDecision != "":
		// Decided already; the CRD rejects any change.
		return admission.Allowed("")
	case s.Decision == "":
		if s.Reviewer == "" && s.ReviewedAt == nil {
			return admission.Allowed("")
		}
		s.Reviewer, s.ReviewedAt = "", nil
	default:
		now := metav1.NewTime(r.now().UTC().Truncate(time.Second))
		s.Reviewer, s.ReviewedAt = req.UserInfo.Username, &now
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

func (r Reviewer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
