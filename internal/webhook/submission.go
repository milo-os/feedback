package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/index"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-submission,mutating=false,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=submissions,verbs=create,versions=v1alpha1,name=submission.feedback.miloapis.com,admissionReviewVersions=v1

// SubmissionCheck admits new submissions from people with a verified email,
// up to Limit per Window each.
type SubmissionCheck struct {
	// Submissions reads submissions, indexed by owner.
	Submissions client.Reader
	// Limit submissions per user per Window. Zero disables the limit.
	Limit  int
	Window time.Duration
	// Now returns the current time; tests replace it.
	Now func() time.Time
	// Attachments reports whether attachments can be accepted.
	Attachments bool
}

var _ admission.Handler = SubmissionCheck{}

// Handle implements admission.Handler.
func (c SubmissionCheck) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}
	if identity.EmailUnverified(req.UserInfo) {
		return admission.Denied("verify your email address to send feedback")
	}
	sub := &v1.Submission{}
	if err := json.Unmarshal(req.Object.Raw, sub); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if len(sub.Spec.Attachments) > 0 && !c.Attachments {
		return admission.Denied("attachments aren't available yet; send the feedback without them")
	}
	if c.Limit <= 0 {
		return admission.Allowed("")
	}
	var mine v1.SubmissionList
	if err := c.Submissions.List(ctx, &mine, client.MatchingFields{index.Owner: sub.Spec.UserRef.Name}); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	now := c.now()
	since := now.Add(-c.Window)
	var recent []metav1.Time
	for _, s := range mine.Items {
		if s.CreationTimestamp.Time.After(since) {
			recent = append(recent, s.CreationTimestamp)
		}
	}
	if len(recent) < c.Limit {
		return admission.Allowed("")
	}
	oldest := recent[0]
	for _, t := range recent {
		if t.Before(&oldest) {
			oldest = t
		}
	}
	wait := oldest.Add(c.Window).Sub(now).Round(time.Second)
	resp := admission.Denied(fmt.Sprintf("You can send %d pieces of feedback every %s; try again in %s.",
		c.Limit, duration.HumanDuration(c.Window), duration.HumanDuration(max(wait, time.Second))))
	resp.Result.Code = http.StatusTooManyRequests
	resp.Result.Reason = metav1.StatusReasonTooManyRequests
	return resp
}

func (c SubmissionCheck) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// +kubebuilder:webhook:path=/validate-submissionreview,mutating=false,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=submissionreviews,verbs=create;update,versions=v1alpha1,name=submissionreview.feedback.miloapis.com,admissionReviewVersions=v1

// ReviewCheck admits a Merged decision only into a post people can see: it
// must exist, not be hidden and not be merged itself.
type ReviewCheck struct {
	Posts client.Reader
}

var _ admission.Handler = ReviewCheck{}

// Handle implements admission.Handler.
func (c ReviewCheck) Handle(ctx context.Context, req admission.Request) admission.Response {
	review := &v1.SubmissionReview{}
	if err := json.Unmarshal(req.Object.Raw, review); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if review.Spec.Decision != v1.ReviewDecisionMerged {
		return admission.Allowed("")
	}
	if req.Operation == admissionv1.Update {
		old := &v1.SubmissionReview{}
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if old.Spec.Decision != "" {
			return admission.Allowed("")
		}
	}
	return checkPost(ctx, c.Posts, review.Spec.MergedInto)
}
