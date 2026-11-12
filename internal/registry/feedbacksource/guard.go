package feedbacksource

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/storage/pg"
	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// FeedbackSources are staff configuration, served only in the platform
// context. Milo IAM already keeps them out of other contexts; this is the
// apiserver's own check.
func platformOnly(ctx context.Context) error {
	if kind, _ := pg.RequestContext(ctx); kind != pg.ScopePlatform {
		return apierrors.NewForbidden(feedbackv1alpha1.Resource("feedbacksources"), "",
			errors.New("feedback sources are only available in the platform context"))
	}
	return nil
}

func (r *REST) Get(ctx context.Context, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.Get(ctx, name, opts)
}

func (r *REST) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.List(ctx, opts)
}

func (r *REST) Watch(ctx context.Context, opts *metainternalversion.ListOptions) (watch.Interface, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.Watch(ctx, opts)
}

func (r *REST) Create(ctx context.Context, obj runtime.Object, v rest.ValidateObjectFunc, opts *metav1.CreateOptions) (runtime.Object, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.Create(ctx, obj, v, opts)
}

func (r *REST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, cv rest.ValidateObjectFunc, uv rest.ValidateObjectUpdateFunc, force bool, opts *metav1.UpdateOptions) (runtime.Object, bool, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, false, err
	}
	return r.Store.Update(ctx, name, objInfo, cv, uv, force, opts)
}

func (r *REST) Delete(ctx context.Context, name string, v rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, false, err
	}
	return r.Store.Delete(ctx, name, v, opts)
}

func (r *REST) DeleteCollection(ctx context.Context, v rest.ValidateObjectFunc, opts *metav1.DeleteOptions, listOpts *metainternalversion.ListOptions) (runtime.Object, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.DeleteCollection(ctx, v, opts, listOpts)
}

func (r *StatusREST) Get(ctx context.Context, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, err
	}
	return r.Store.Get(ctx, name, opts)
}

func (r *StatusREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, cv rest.ValidateObjectFunc, uv rest.ValidateObjectUpdateFunc, force bool, opts *metav1.UpdateOptions) (runtime.Object, bool, error) {
	if err := platformOnly(ctx); err != nil {
		return nil, false, err
	}
	return r.Store.Update(ctx, name, objInfo, cv, uv, force, opts)
}
