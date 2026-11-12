package pg

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
)

// Column is an extra column value copied from the object on write.
type Column struct {
	Name  string
	Value any
}

// Kind describes how one resource maps onto its table.
type Kind struct {
	// Table is the table name. It comes from code, never from a request.
	Table string

	NewFunc     func() runtime.Object
	NewListFunc func() runtime.Object

	DefaultQualifiedResource  schema.GroupResource
	SingularQualifiedResource schema.GroupResource

	CreateStrategy rest.RESTCreateStrategy
	UpdateStrategy rest.RESTUpdateStrategy
	DeleteStrategy rest.RESTDeleteStrategy

	// Scope resolves which owner a request reads and writes.
	Scope ScopeFunc
	// CreateScope, if set, resolves the owner of a new object instead of
	// Scope. Security reports use it: staff read every report, but the ones
	// they log for external reporters belong to the platform.
	CreateScope ScopeFunc

	// Columns returns the extra column values for obj. Every call must return
	// the same column names in the same order.
	Columns func(obj runtime.Object) []Column

	// FieldColumns maps field selector labels to columns. metadata.name is
	// always supported.
	FieldColumns map[string]string

	// TableConvertor renders kubectl tables. Defaults to name and age.
	TableConvertor rest.TableConvertor

	// ReadFilter optionally returns an extra SQL condition (no parameters)
	// that every read, update and delete must match. Rows it excludes are
	// NotFound to the caller. Used to hide hidden posts outside the platform
	// context.
	ReadFilter func(ctx context.Context) string

	// BeforeWrite runs inside the write's transaction after validation and
	// before the row is written. old is nil on create and obj is nil on
	// delete. It may change obj (for server-computed fields); an error aborts
	// the write.
	BeforeWrite func(ctx context.Context, op Op, old, obj runtime.Object) error

	// AfterWrite runs inside the write's transaction after the row is
	// written, for side effects that must commit with it.
	AfterWrite func(ctx context.Context, op Op, old, obj runtime.Object) error

	// Decorate fills derived fields on objects about to be returned.
	Decorate func(ctx context.Context, objs []runtime.Object) error
}

// Op is a write operation passed to the write hooks.
type Op string

const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

func (k Kind) validate() error {
	var errs []error
	if k.Table == "" {
		errs = append(errs, errors.New("Table is required"))
	}
	if k.NewFunc == nil || k.NewListFunc == nil {
		errs = append(errs, errors.New("NewFunc and NewListFunc are required"))
	}
	if k.DefaultQualifiedResource.Empty() {
		errs = append(errs, errors.New("DefaultQualifiedResource is required"))
	}
	if k.CreateStrategy == nil || k.UpdateStrategy == nil || k.DeleteStrategy == nil {
		errs = append(errs, errors.New("create, update and delete strategies are required"))
	}
	if k.Scope == nil {
		errs = append(errs, errors.New("Scope is required"))
	}
	return errors.Join(errs...)
}

func (k Kind) columns(obj runtime.Object) []Column {
	if k.Columns == nil {
		return nil
	}
	return k.Columns(obj)
}

func (k Kind) beforeWrite(ctx context.Context, op Op, old, obj runtime.Object) error {
	if k.BeforeWrite == nil {
		return nil
	}
	return k.BeforeWrite(ctx, op, old, obj)
}

func (k Kind) afterWrite(ctx context.Context, op Op, old, obj runtime.Object) error {
	if k.AfterWrite == nil {
		return nil
	}
	return k.AfterWrite(ctx, op, old, obj)
}

func (k Kind) decorate(ctx context.Context, objs ...runtime.Object) error {
	if k.Decorate == nil || len(objs) == 0 {
		return nil
	}
	return k.Decorate(ctx, objs)
}

func (k Kind) readFilter(ctx context.Context, w *where) {
	if k.ReadFilter == nil {
		return
	}
	if c := k.ReadFilter(ctx); c != "" {
		w.conds = append(w.conds, "("+c+")")
	}
}
