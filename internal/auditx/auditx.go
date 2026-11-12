// Package auditx records a request's side effects as audit annotations, so
// the request's own audit event shows everything it changed without
// carrying private content.
package auditx

import (
	"context"

	"k8s.io/apiserver/pkg/audit"
)

// Annotation keys.
const (
	// Subscribed is the post a vote subscribed its voter to.
	Subscribed = "feedback.miloapis.com/subscribed"
	// VotesMovedTo is the post a merged post's votes moved to.
	VotesMovedTo = "feedback.miloapis.com/votes-moved-to"
	// VoteGranted is "<user>/<post>" when a review or a linked post gave the
	// submitter a vote.
	VoteGranted = "feedback.miloapis.com/vote-granted"
	// SecurityReport is the report a submission became.
	SecurityReport = "feedback.miloapis.com/security-report"
)

// Annotate adds key=value to the request's audit event, if it has one.
func Annotate(ctx context.Context, key, value string) {
	audit.AddAuditAnnotation(ctx, key, value)
}
