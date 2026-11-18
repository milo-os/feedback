package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.miloapis.com/feedback/internal/identity"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-vote,mutating=false,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=votes;subscriptions,verbs=create,versions=v1alpha1,name=vote.feedback.miloapis.com,admissionReviewVersions=v1

// VoteCheck admits votes and follows only on posts people can see: the post
// must exist, not be hidden and not be merged into another. Votes also need
// a verified email address.
type VoteCheck struct {
	// Posts reads posts. The operator's cache sees hidden ones.
	Posts client.Reader
}

var _ admission.Handler = VoteCheck{}

// Handle implements admission.Handler.
func (c VoteCheck) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}
	if req.Resource.Resource == "votes" && identity.EmailUnverified(req.UserInfo) {
		return admission.Denied("verify your email address to vote")
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, &obj.Object); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "postRef", "name")
	return checkPost(ctx, c.Posts, name)
}

// checkPost admits a reference to a post that exists, isn't hidden and isn't
// merged into another.
func checkPost(ctx context.Context, posts client.Reader, name string) admission.Response {
	var post v1.Post
	err := posts.Get(ctx, types.NamespacedName{Name: name}, &post)
	switch {
	case apierrors.IsNotFound(err) || (err == nil && post.Spec.Hidden):
		return admission.Denied(fmt.Sprintf("post %q not found", name))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	case post.Status.MergedInto != "":
		return admission.Denied(fmt.Sprintf("post %q was merged into %q; use that one", name, post.Status.MergedInto))
	}
	return admission.Allowed("")
}
