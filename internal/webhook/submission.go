package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
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
//
// The limit is soft. It counts the user's submissions in the operator's
// cache, which lags creation slightly, and each operator replica keeps its
// own count. Admitted (when set) closes the gap within one replica by
// remembering what this replica admitted that the cache hasn't seen yet, so
// a burst of parallel requests to one replica gets at most Limit through.
// Requests spread across replicas can still get Limit through each, so a
// determined user can send replicas × Limit per Window. That is acceptable
// here: submissions are never published without a staff review, so the
// limit protects the review queue rather than anything public. A strict
// limit would need a shared counter written on every submission.
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
	// Admitted remembers recent admissions on this replica. Nil disables it.
	Admitted *Admitted
}

// Admitted records when this replica admitted each user's submissions.
type Admitted struct {
	mu     sync.Mutex
	byUser map[string][]time.Time
}

// NewAdmitted returns an empty record.
func NewAdmitted() *Admitted { return &Admitted{byUser: map[string][]time.Time{}} }

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
	owner := sub.Spec.UserRef.Name
	if c.Admitted != nil {
		// Decide one request at a time, so parallel requests see each
		// other's admissions.
		c.Admitted.mu.Lock()
		defer c.Admitted.mu.Unlock()
	}
	var mine v1.SubmissionList
	if err := c.Submissions.List(ctx, &mine, client.MatchingFields{index.Owner: owner}); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	now := c.now()
	since := now.Add(-c.Window)
	var recent []time.Time
	var newest time.Time
	for _, s := range mine.Items {
		t := s.CreationTimestamp.Time
		if t.After(since) {
			recent = append(recent, t)
		}
		if t.After(newest) {
			newest = t
		}
	}
	if c.Admitted != nil {
		// A submission is created after it is admitted, so an admission
		// older than the newest submission in the cache has been counted.
		var keep []time.Time
		for _, t := range c.Admitted.byUser[owner] {
			if t.After(since) {
				keep = append(keep, t)
				if t.After(newest) {
					recent = append(recent, t)
				}
			}
		}
		c.Admitted.byUser[owner] = keep
	}
	if len(recent) < c.Limit {
		if c.Admitted != nil {
			c.Admitted.byUser[owner] = append(c.Admitted.byUser[owner], now)
		}
		return admission.Allowed("")
	}
	oldest := recent[0]
	for _, t := range recent {
		if t.Before(oldest) {
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
