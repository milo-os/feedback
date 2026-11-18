package webhook

import (
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// Register adds every admission webhook to mgr's webhook server.
func Register(mgr manager.Manager) {
	s := mgr.GetWebhookServer()
	s.Register("/mutate-owner", &webhook.Admission{Handler: Owner{}})
	s.Register("/mutate-submissionreview", &webhook.Admission{Handler: Reviewer{}})
	s.Register("/validate-vote", &webhook.Admission{Handler: VoteCheck{Posts: mgr.GetClient()}})
}
