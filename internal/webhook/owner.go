// Package webhook holds the operator's admission webhooks, which Milo calls
// for feedback resources.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.miloapis.com/feedback/internal/identity"
)

// +kubebuilder:webhook:path=/mutate-owner,mutating=true,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=votes;subscriptions;submissions;securityreports;feedbackprofiles,verbs=create,versions=v1alpha1,name=owner.feedback.miloapis.com,admissionReviewVersions=v1

// Owner sets spec.userRef on user-owned resources from the request, so no
// client can create one for someone else. Milo limits what each user sees by
// that field.
//
//   - In a user's own context, spec.userRef is that user, whatever the client
//     sent.
//   - The operator creates them for users in the platform context (a vote for
//     an approved submitter, a report moved from a submission) and names the
//     owner itself.
//   - The Security Team logs reports for people without an account in the
//     platform context, with no owner.
//
// Anything else is refused. Updates can't change spec.userRef: the CRDs make
// it immutable.
type Owner struct{}

var _ admission.Handler = Owner{}

// Handle implements admission.Handler.
func (Owner) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, &obj.Object); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	owner, _, _ := unstructured.NestedString(obj.Object, "spec", "userRef", "name")
	resource := req.Resource.Resource

	if user, ok := identity.User(req.UserInfo); ok {
		if owner == user {
			return admission.Allowed("")
		}
		if err := unstructured.SetNestedField(obj.Object, user, "spec", "userRef", "name"); err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return patch(req, obj)
	}

	switch {
	case identity.Of(req.UserInfo) == identity.Operator:
		if owner == "" && resource != "securityreports" {
			return admission.Denied("spec.userRef.name is required")
		}
		return admission.Allowed("")
	case resource == "securityreports" && owner == "":
		// The Security Team logging a report from email or GitHub.
		return admission.Allowed("")
	case resource == "securityreports":
		return admission.Denied("in the platform context, reports are logged for external reporters only: leave spec.userRef empty and set spec.reporter")
	}
	return admission.Denied(fmt.Sprintf("%s are created by users in their own context", resource))
}

func patch(req admission.Request, obj *unstructured.Unstructured) admission.Response {
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
