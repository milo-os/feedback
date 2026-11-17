// Package index registers the cache field indexes the reconcilers and
// webhooks share.
package index

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const (
	// Post indexes votes and subscriptions by spec.postRef.name.
	Post = "spec.postRef.name"
	// Owner indexes user-owned objects by spec.userRef.name.
	Owner = "spec.userRef.name"
	// FromSubmission indexes posts by the submission they were created from
	// (v1alpha1.FromSubmissionAnnotation).
	FromSubmission = "metadata.annotations.fromSubmission"
)

// Add registers every index with mgr. Call it once, before mgr starts.
func Add(ctx context.Context, mgr manager.Manager) error {
	idx := mgr.GetFieldIndexer()
	for _, i := range []struct {
		obj   client.Object
		field string
		fn    client.IndexerFunc
	}{
		{&v1.Vote{}, Post, func(o client.Object) []string { return []string{o.(*v1.Vote).Spec.PostRef.Name} }},
		{&v1.Subscription{}, Post, func(o client.Object) []string { return []string{o.(*v1.Subscription).Spec.PostRef.Name} }},
		{&v1.Submission{}, Owner, func(o client.Object) []string { return []string{o.(*v1.Submission).Spec.UserRef.Name} }},
		{&v1.Post{}, FromSubmission, func(o client.Object) []string {
			if s := o.GetAnnotations()[v1.FromSubmissionAnnotation]; s != "" {
				return []string{s}
			}
			return nil
		}},
	} {
		if err := idx.IndexField(ctx, i.obj, i.field, i.fn); err != nil {
			return err
		}
	}
	return nil
}
