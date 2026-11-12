package vote

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"

	"go.miloapis.com/feedback/internal/auditx"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// SubscriptionREST is Subscription storage.
type SubscriptionREST struct {
	*pg.Store
}

// ValidReasons lists every SubscriptionReason.
var ValidReasons = []v1.SubscriptionReason{
	v1.SubscriptionReasonVoted, v1.SubscriptionReasonSubmitted,
	v1.SubscriptionReasonMerged, v1.SubscriptionReasonManual,
}

type reasonKey struct{}

type subscriptionStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func (subscriptionStrategy) NamespaceScoped() bool { return false }

// PrepareForCreate sets the reason. Users following a post themselves get
// "manual"; the other reasons are set by the server when it subscribes them.
func (subscriptionStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	s := obj.(*v1.Subscription)
	s.Spec.Reason = v1.SubscriptionReasonManual
	if r, ok := ctx.Value(reasonKey{}).(v1.SubscriptionReason); ok {
		s.Spec.Reason = r
	}
}

func (subscriptionStrategy) PrepareForUpdate(context.Context, runtime.Object, runtime.Object) {}

func (subscriptionStrategy) Validate(_ context.Context, obj runtime.Object) field.ErrorList {
	s := obj.(*v1.Subscription)
	errs := validateRef(s.Name, s.Spec.PostRef)
	if r := s.Spec.Reason; !slices.Contains(ValidReasons, r) {
		errs = append(errs, field.NotSupported(field.NewPath("spec", "reason"), r, ValidReasons))
	}
	return errs
}

func (s subscriptionStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	errs := s.Validate(ctx, obj)
	if !equality.Semantic.DeepEqual(obj.(*v1.Subscription).Spec, old.(*v1.Subscription).Spec) {
		errs = append(errs, field.Forbidden(field.NewPath("spec"), "is immutable"))
	}
	return errs
}

func (subscriptionStrategy) WarningsOnCreate(context.Context, runtime.Object) []string { return nil }
func (subscriptionStrategy) WarningsOnUpdate(context.Context, runtime.Object, runtime.Object) []string {
	return nil
}
func (subscriptionStrategy) AllowCreateOnUpdate(context.Context) bool      { return false }
func (subscriptionStrategy) AllowUnconditionalUpdate(context.Context) bool { return true }
func (subscriptionStrategy) Canonicalize(runtime.Object)                   {}

// NewSubscriptionREST returns Subscription storage.
func NewSubscriptionREST(db *pg.DB, typer runtime.ObjectTyper) (*SubscriptionREST, error) {
	s := subscriptionStrategy{ObjectTyper: typer, NameGenerator: names.SimpleNameGenerator}
	store, err := pg.NewStore(db, pg.Kind{
		Table:                     "subscriptions",
		NewFunc:                   func() runtime.Object { return &v1.Subscription{} },
		NewListFunc:               func() runtime.Object { return &v1.SubscriptionList{} },
		DefaultQualifiedResource:  v1.Resource("subscriptions"),
		SingularQualifiedResource: v1.Resource("subscription"),
		CreateStrategy:            s, UpdateStrategy: s, DeleteStrategy: s,
		Scope: pg.UserOwned,
		Columns: func(obj runtime.Object) []pg.Column {
			sub := obj.(*v1.Subscription)
			return []pg.Column{
				{Name: "post_name", Value: sub.Spec.PostRef.Name},
				{Name: "reason", Value: string(sub.Spec.Reason)},
			}
		},
		FieldColumns: map[string]string{"spec.postRef.name": "post_name", "spec.reason": "reason"},
		BeforeWrite: func(ctx context.Context, op pg.Op, _, obj runtime.Object) error {
			if op != pg.OpCreate {
				return userOnly(ctx)
			}
			sub := obj.(*v1.Subscription)
			return CheckPost(ctx, db, v1.Kind("Subscription"), sub.Name, sub.Spec.PostRef.Name)
		},
		TableConvertor: subscriptionTable{},
	})
	if err != nil {
		return nil, err
	}
	return &SubscriptionREST{Store: store}, nil
}

// Follow subscribes the caller to postName, unless they already are. It runs
// in ctx's transaction, in a savepoint so an existing subscription doesn't
// abort it.
func (r *SubscriptionREST) Follow(ctx context.Context, postName string, reason v1.SubscriptionReason) error {
	sub := &v1.Subscription{
		ObjectMeta: metav1.ObjectMeta{Name: postName},
		Spec:       v1.SubscriptionSpec{PostRef: v1.PostReference{Name: postName}},
	}
	_, err := r.Create(context.WithValue(ctx, reasonKey{}, reason), sub, nil, &metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err == nil {
		auditx.Annotate(ctx, auditx.Subscribed, postName)
	}
	return err
}

type subscriptionTable struct{}

func (subscriptionTable) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Owner", Type: "string", Priority: 1},
		{Name: "Reason", Type: "string"},
		{Name: "Age", Type: "date"},
	}}
	add := func(s *v1.Subscription) {
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: s},
			Cells:  []any{s.Name, s.Annotations[pg.OwnerAnnotation], string(s.Spec.Reason), age(s.CreationTimestamp)},
		})
	}
	switch o := obj.(type) {
	case *v1.Subscription:
		add(o)
	case *v1.SubscriptionList:
		t.ResourceVersion, t.Continue = o.ResourceVersion, o.Continue
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}
