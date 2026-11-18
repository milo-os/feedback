package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
)

// +kubebuilder:webhook:path=/mutate-attachments,mutating=true,failurePolicy=fail,sideEffects=None,groups=feedback.miloapis.com,resources=submissions;securityreports,verbs=create,versions=v1alpha1,name=attachments.feedback.miloapis.com,admissionReviewVersions=v1

// Attachments checks that every attachment on a new submission or security
// report is one of the owner's uploads for that purpose, and sets each
// attachment's name and type from the upload rather than trusting the
// client. The operator's own creates (a report moved from a submission)
// carry files it has already copied, and pass.
type Attachments struct {
	// Files is the attachments bucket. Nil refuses attachments.
	Files *attachments.Store
}

var _ admission.Handler = Attachments{}

// Handle implements admission.Handler.
func (a Attachments) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create || identity.Of(req.UserInfo) == identity.Operator {
		return admission.Allowed("")
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.Object.Raw, &obj.Object); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	list, _, _ := unstructured.NestedSlice(obj.Object, "spec", "attachments")
	if len(list) == 0 {
		return admission.Allowed("")
	}
	if a.Files == nil {
		return admission.Denied("attachments aren't available; send it without them")
	}
	purpose, _ := attachments.PurposeOf(req.Resource.Resource)
	owner, ok := identity.User(req.UserInfo)
	if !ok {
		return admission.Denied("attachments can only be added by their uploader, in their own user context")
	}
	refs := make([]string, 0, len(list))
	for _, item := range list {
		ref, _, _ := unstructured.NestedString(item.(map[string]any), "uploadRef")
		refs = append(refs, ref)
	}
	found, err := a.Files.Lookup(ctx, purpose, owner, refs)
	if errors.Is(err, attachments.ErrNotFound) {
		return admission.Denied("every uploadRef must be one of your uploads for this from the last day")
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "Looking up uploads failed")
		return admission.Errored(http.StatusServiceUnavailable, errors.New("attachments aren't available right now; try again or send it without them"))
	}
	out := make([]any, len(found))
	for i, f := range found {
		out[i] = map[string]any{"name": f.Name, "contentType": f.ContentType, "uploadRef": f.UploadRef}
	}
	if err := unstructured.SetNestedSlice(obj.Object, out, "spec", "attachments"); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return patch(req, obj)
}
