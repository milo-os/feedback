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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.miloapis.com/feedback/internal/milo"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// FieldManager is the operator's server-side apply field manager.
const FieldManager = "feedback-operator"

// postIndex indexes votes and subscriptions by spec.postRef.name.
const postIndex = "spec.postRef.name"

// Options configures the reconcilers.
type Options struct {
	// Users looks up voters' organizations and GitHub usernames. Nil skips
	// the lookups.
	Users milo.Users
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Setup adds the field indexes and every reconciler to mgr.
func Setup(ctx context.Context, mgr ctrl.Manager, o Options) error {
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &v1.Vote{}, postIndex, func(obj client.Object) []string {
		return []string{obj.(*v1.Vote).Spec.PostRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &v1.Subscription{}, postIndex, func(obj client.Object) []string {
		return []string{obj.(*v1.Subscription).Spec.PostRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &v1.Post{}, mergedIntoIndex, func(obj client.Object) []string {
		if into := obj.(*v1.Post).Status.MergedInto; into != "" {
			return []string{into}
		}
		return nil
	}); err != nil {
		return err
	}
	c := mgr.GetClient()
	for _, r := range []interface{ SetupWithManager(ctrl.Manager) error }{
		&PostVotesReconciler{Client: c, Options: o},
		&VoteReconciler{Client: c, Options: o},
		&MergeReconciler{Client: c},
	} {
		if err := r.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}
