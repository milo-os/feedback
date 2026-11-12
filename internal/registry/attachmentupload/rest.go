// Package attachmentupload serves AttachmentUpload: a create-only resource
// that stores a file for a later Submission or SecurityReport. Nothing is
// stored as an API object.
package attachmentupload

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/identity"
	"go.miloapis.com/feedback/internal/ratelimit"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

var purposes = []v1.AttachmentPurpose{v1.AttachmentPurposeSubmission, v1.AttachmentPurposeSecurityReport}

// REST serves attachmentuploads.
type REST struct {
	files *attachments.Store // nil when no bucket is configured
	limit *ratelimit.Limiter
	table rest.TableConvertor
}

var (
	_ rest.Storage              = &REST{}
	_ rest.Creater              = &REST{}
	_ rest.Scoper               = &REST{}
	_ rest.SingularNameProvider = &REST{}
)

// NewREST returns AttachmentUpload storage. files may be nil, in which case
// uploads are unavailable.
func NewREST(files *attachments.Store, limit *ratelimit.Limiter) *REST {
	return &REST{files: files, limit: limit, table: rest.NewDefaultTableConvertor(v1.Resource("attachmentuploads"))}
}

func (r *REST) New() runtime.Object     { return &v1.AttachmentUpload{} }
func (r *REST) Destroy()                {}
func (r *REST) NamespaceScoped() bool   { return false }
func (r *REST) GetSingularName() string { return "attachmentupload" }
func (r *REST) ConvertToTable(ctx context.Context, obj, opts runtime.Object) (*metav1.Table, error) {
	return r.table.ConvertToTable(ctx, obj, opts)
}

// Create stores the file and returns its reference. The response leaves out
// the data.
func (r *REST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	up, ok := obj.(*v1.AttachmentUpload)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not an AttachmentUpload: %T", obj))
	}
	gr := v1.Resource("attachmentuploads")
	kind, owner := pg.RequestContext(ctx)
	if kind != pg.ScopeUser {
		return nil, apierrors.NewForbidden(gr, "", errors.New("upload attachments from the user context"))
	}
	if identity.EmailUnverified(ctx) {
		return nil, apierrors.NewForbidden(gr, "", errors.New("verify your email address to upload attachments"))
	}
	if r.files == nil {
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}
	s := up.Spec
	p := field.NewPath("spec")
	var errs field.ErrorList
	if !slices.Contains(purposes, s.Purpose) {
		errs = append(errs, field.NotSupported(p.Child("purpose"), s.Purpose, purposes))
	}
	if msg := r.files.Check(s.Name, s.ContentType, s.Data); msg != "" {
		errs = append(errs, field.Invalid(p, s.Name, msg))
	}
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(v1.Kind("AttachmentUpload"), up.Name, errs)
	}
	if ok, wait := r.limit.Allow(owner); !ok {
		return nil, apierrors.NewTooManyRequests("Too many uploads; try again shortly.", int(math.Ceil(wait.Seconds())))
	}

	ref, sum, err := r.files.Upload(ctx, s.Purpose, owner, s.Name, s.ContentType, s.Data)
	if err != nil {
		klog.ErrorS(err, "Storing attachment failed")
		return nil, apierrors.NewServiceUnavailable("Attachments are not available.")
	}
	out := up.DeepCopy()
	out.Name, out.GenerateName = ref, ""
	out.CreationTimestamp = metav1.Now()
	out.Spec.Data = nil
	out.Status = v1.AttachmentUploadStatus{UploadRef: ref, Size: int64(len(s.Data)), SHA256: sum}
	return out, nil
}
