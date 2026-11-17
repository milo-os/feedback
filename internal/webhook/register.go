package webhook

import (
	"time"

	"go.miloapis.com/feedback/internal/attachments"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// Options configures the webhooks.
type Options struct {
	// SubmissionLimit submissions per user per SubmissionWindow. Zero
	// disables the limit.
	SubmissionLimit  int
	SubmissionWindow time.Duration
	// Files is the attachments bucket. Nil refuses attachments.
	Files *attachments.Store
}

// Register adds every admission webhook to mgr's webhook server. The indexes
// in package index must be added too.
func Register(mgr manager.Manager, o Options) {
	s := mgr.GetWebhookServer()
	c := mgr.GetClient()
	s.Register("/mutate-owner", &webhook.Admission{Handler: Owner{}})
	s.Register("/mutate-submissionreview", &webhook.Admission{Handler: Reviewer{}})
	s.Register("/mutate-attachments", &webhook.Admission{Handler: Attachments{Files: o.Files}})
	s.Register("/validate-vote", &webhook.Admission{Handler: VoteCheck{Posts: c}})
	s.Register("/validate-submission", &webhook.Admission{Handler: SubmissionCheck{
		Submissions: c, Limit: o.SubmissionLimit, Window: o.SubmissionWindow,
	}})
	s.Register("/validate-submissionreview", &webhook.Admission{Handler: ReviewCheck{Posts: c}})
}
