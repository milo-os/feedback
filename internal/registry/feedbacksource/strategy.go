// Package feedbacksource implements etcd-backed REST storage for FeedbackSource.
package feedbacksource

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

type strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// NewStrategy returns the create/update/delete strategy for FeedbackSource.
func NewStrategy(typer runtime.ObjectTyper) strategy {
	return strategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
}

func (strategy) NamespaceScoped() bool { return false }

func (strategy) PrepareForCreate(_ context.Context, obj runtime.Object) {
	src := obj.(*feedbackv1alpha1.FeedbackSource)
	src.Status = feedbackv1alpha1.FeedbackSourceStatus{}
	src.Generation = 1
}

func (strategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	newSrc := obj.(*feedbackv1alpha1.FeedbackSource)
	oldSrc := old.(*feedbackv1alpha1.FeedbackSource)
	newSrc.Status = oldSrc.Status
	newSrc.Generation = oldSrc.Generation
	if !equality.Semantic.DeepEqual(newSrc.Spec, oldSrc.Spec) {
		newSrc.Generation++
	}
}

func (strategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	return Validate(obj.(*feedbackv1alpha1.FeedbackSource))
}

func (strategy) ValidateUpdate(_ context.Context, obj, old runtime.Object) field.ErrorList {
	newSrc := obj.(*feedbackv1alpha1.FeedbackSource)
	oldSrc := old.(*feedbackv1alpha1.FeedbackSource)
	errs := Validate(newSrc)
	if newSrc.Spec.GitHub.Org != oldSrc.Spec.GitHub.Org {
		errs = append(errs, field.Invalid(field.NewPath("spec", "github", "org"),
			newSrc.Spec.GitHub.Org, "org is immutable; create a new FeedbackSource instead"))
	}
	return errs
}

func (strategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (strategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (strategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (strategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (strategy) Canonicalize(runtime.Object)                   {}

// Validate checks a FeedbackSource spec.
func Validate(src *feedbackv1alpha1.FeedbackSource) field.ErrorList {
	var errs field.ErrorList
	gh := field.NewPath("spec", "github")
	spec := src.Spec

	if spec.GitHub.Org == "" {
		errs = append(errs, field.Required(gh.Child("org"), "the GitHub organization login"))
	}
	if spec.GitHub.InstallationID <= 0 {
		errs = append(errs, field.Invalid(gh.Child("installationID"), spec.GitHub.InstallationID,
			"must be the GitHub App installation ID for the org"))
	}
	if p := spec.GitHub.Project; p != nil {
		pp := gh.Child("project")
		if p.Owner == "" {
			errs = append(errs, field.Required(pp.Child("owner"), ""))
		}
		if p.Number <= 0 {
			errs = append(errs, field.Invalid(pp.Child("number"), p.Number, "must be a positive project number"))
		}
	}
	if m := spec.GitHub.Milestones; m != nil && m.Repository == "" {
		errs = append(errs, field.Required(gh.Child("milestones", "repository"), ""))
	}
	if c := spec.GitHub.Changelog; c != nil {
		cp := gh.Child("changelog")
		if c.Repository == "" {
			errs = append(errs, field.Required(cp.Child("repository"), ""))
		}
		if c.DiscussionCategory == "" {
			errs = append(errs, field.Required(cp.Child("discussionCategory"), ""))
		}
	}
	for k, v := range spec.StatusMapping {
		if !slices.Contains(feedbackv1alpha1.ValidPostStatuses, v) {
			errs = append(errs, field.NotSupported(field.NewPath("spec", "statusMapping").Key(k), v,
				feedbackv1alpha1.ValidPostStatuses))
		}
	}
	for k, v := range spec.CategoryMapping {
		if v == "" {
			errs = append(errs, field.Required(field.NewPath("spec", "categoryMapping").Key(k), "category name"))
		}
	}
	return errs
}

// statusStrategy only allows status to change.
type statusStrategy struct {
	strategy
}

func (statusStrategy) PrepareForUpdate(_ context.Context, obj, old runtime.Object) {
	newSrc := obj.(*feedbackv1alpha1.FeedbackSource)
	oldSrc := old.(*feedbackv1alpha1.FeedbackSource)
	newSrc.Spec = oldSrc.Spec
	newSrc.Generation = oldSrc.Generation
	newSrc.Labels = oldSrc.Labels
	newSrc.Annotations = oldSrc.Annotations
}

func (statusStrategy) ValidateUpdate(context.Context, runtime.Object, runtime.Object) field.ErrorList {
	return nil
}

func (statusStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return map[fieldpath.APIVersion]*fieldpath.Set{
		fieldpath.APIVersion(feedbackv1alpha1.SchemeGroupVersion.String()): fieldpath.NewSet(
			fieldpath.MakePathOrDie("spec"),
		),
	}
}

// GetAttrs returns the labels and fields used for selectors.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	src, ok := obj.(*feedbackv1alpha1.FeedbackSource)
	if !ok {
		return nil, nil, fmt.Errorf("not a FeedbackSource: %T", obj)
	}
	fs := generic.ObjectMetaFieldsSet(&src.ObjectMeta, false)
	fs["spec.github.org"] = src.Spec.GitHub.Org
	return src.Labels, fs, nil
}

// Match returns a selection predicate for FeedbackSource.
func Match(label labels.Selector, f fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{Label: label, Field: f, GetAttrs: GetAttrs}
}
