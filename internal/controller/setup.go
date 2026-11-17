// Package controller holds the operator's reconcilers. Each one makes
// something that follows from a request true shortly after it: a follow for
// a new vote, a post's vote totals, votes moved off a merged post.
//
// Every step is idempotent and keyed on deterministic names, so a crash or a
// retry never duplicates a vote or a follow.
package controller

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/milo"
)

// FieldManager is the operator's server-side apply field manager.
const FieldManager = "feedback-operator"

// Options configures the reconcilers.
type Options struct {
	// Users looks up voters' organizations and GitHub usernames. Nil skips
	// the lookups.
	Users milo.Users
	// Now returns the current time; tests replace it.
	Now func() time.Time
	// Files is the attachments bucket. Nil leaves attachments alone.
	Files *attachments.Store
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Setup adds every reconciler to mgr. The indexes in package index must be
// added first.
func Setup(ctx context.Context, mgr ctrl.Manager, o Options) error {
	c := mgr.GetClient()
	for _, r := range []interface{ SetupWithManager(ctrl.Manager) error }{
		&PostVotesReconciler{Client: c, Options: o},
		&VoteReconciler{Client: c, Options: o},
		&MergeReconciler{Client: c},
		&SubmissionReconciler{Client: c, Files: o.Files},
		&ReviewReconciler{Client: c},
		&SecurityReportReconciler{Client: c, Options: o},
		&MoveReconciler{Client: c, Options: o},
	} {
		if err := r.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}
