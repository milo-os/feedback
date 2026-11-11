package pg

import (
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

	// Columns returns the extra column values for obj. Every call must return
	// the same column names in the same order.
	Columns func(obj runtime.Object) []Column

	// FieldColumns maps field selector labels to columns. metadata.name is
	// always supported.
	FieldColumns map[string]string

	// TableConvertor renders kubectl tables. Defaults to name and age.
	TableConvertor rest.TableConvertor
}

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
