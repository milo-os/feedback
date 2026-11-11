package feedbacksource

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	feedbackv1alpha1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// REST is the storage for FeedbackSource and its status subresource.
type REST struct {
	*genericregistry.Store
	Status *StatusREST
}

// StatusREST serves the status subresource.
type StatusREST struct {
	*genericregistry.Store
}

// NewREST returns etcd-backed storage for FeedbackSource.
func NewREST(scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (*REST, error) {
	s := NewStrategy(scheme)

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &feedbackv1alpha1.FeedbackSource{} },
		NewListFunc:               func() runtime.Object { return &feedbackv1alpha1.FeedbackSourceList{} },
		PredicateFunc:             Match,
		DefaultQualifiedResource:  feedbackv1alpha1.Resource("feedbacksources"),
		SingularQualifiedResource: feedbackv1alpha1.Resource("feedbacksource"),

		CreateStrategy: s,
		UpdateStrategy: s,
		DeleteStrategy: s,

		TableConvertor: tableConvertor{},
	}
	if err := store.CompleteWithOptions(&generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: GetAttrs}); err != nil {
		return nil, err
	}

	statusStore := *store
	ss := statusStrategy{s}
	statusStore.UpdateStrategy = ss
	statusStore.ResetFieldsStrategy = ss

	return &REST{Store: store, Status: &StatusREST{Store: &statusStore}}, nil
}

type tableConvertor struct{}

var _ rest.TableConvertor = tableConvertor{}

func (tableConvertor) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Org", Type: "string", Description: "GitHub organization"},
			{Name: "Ready", Type: "string"},
			{Name: "Last Reconcile", Type: "string"},
			{Name: "Age", Type: "string"},
		},
	}
	switch t := obj.(type) {
	case *feedbackv1alpha1.FeedbackSource:
		table.Rows = append(table.Rows, row(t))
	case *feedbackv1alpha1.FeedbackSourceList:
		for i := range t.Items {
			table.Rows = append(table.Rows, row(&t.Items[i]))
		}
	}
	return table, nil
}

func row(src *feedbackv1alpha1.FeedbackSource) metav1.TableRow {
	ready := "Unknown"
	if c := apimeta.FindStatusCondition(src.Status.Conditions, "Ready"); c != nil {
		ready = string(c.Status)
	}
	lastReconcile := "<never>"
	if lr := src.Status.LastReconcile; lr != nil && !lr.CompletedAt.IsZero() {
		lastReconcile = duration.HumanDuration(metav1.Now().Sub(lr.CompletedAt.Time)) + " ago"
	}
	age := "<unknown>"
	if !src.CreationTimestamp.IsZero() {
		age = duration.HumanDuration(metav1.Now().Sub(src.CreationTimestamp.Time))
	}
	return metav1.TableRow{
		Object: runtime.RawExtension{Object: src},
		Cells:  []any{src.Name, src.Spec.GitHub.Org, ready, lastReconcile, age},
	}
}
