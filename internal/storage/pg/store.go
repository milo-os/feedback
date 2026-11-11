package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// Store is rest.Storage for one kind, backed by its Postgres table. It
// supports get, list, create, update (and so patch and apply) and delete.
// Watch and deletecollection are not supported.
type Store struct {
	db   *DB
	kind Kind
}

var (
	_ rest.Storage              = (*Store)(nil)
	_ rest.Scoper               = (*Store)(nil)
	_ rest.SingularNameProvider = (*Store)(nil)
	_ rest.Getter               = (*Store)(nil)
	_ rest.Lister               = (*Store)(nil)
	_ rest.Creater              = (*Store)(nil)
	_ rest.Updater              = (*Store)(nil)
	_ rest.GracefulDeleter      = (*Store)(nil)
	_ rest.ResetFieldsStrategy  = (*Store)(nil)
)

// NewStore returns storage for kind.
func NewStore(db *DB, kind Kind) (*Store, error) {
	if err := kind.validate(); err != nil {
		return nil, fmt.Errorf("invalid kind %s: %w", kind.DefaultQualifiedResource, err)
	}
	if kind.TableConvertor == nil {
		kind.TableConvertor = rest.NewDefaultTableConvertor(kind.DefaultQualifiedResource)
	}
	return &Store{db: db, kind: kind}, nil
}

// DB returns the database, for side effects that must share a request's
// transaction.
func (s *Store) DB() *DB { return s.db }

func (s *Store) New() runtime.Object     { return s.kind.NewFunc() }
func (s *Store) NewList() runtime.Object { return s.kind.NewListFunc() }
func (s *Store) Destroy()                {}
func (s *Store) NamespaceScoped() bool   { return false }

func (s *Store) GetSingularName() string {
	return s.kind.SingularQualifiedResource.Resource
}

func (s *Store) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.kind.TableConvertor.ConvertToTable(ctx, obj, opts)
}

// GetResetFields delegates to the update strategy, so server-side apply on
// the main resource ignores status, and on status ignores spec.
func (s *Store) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	if r, ok := s.kind.UpdateStrategy.(rest.ResetFieldsStrategy); ok {
		return r.GetResetFields()
	}
	return nil
}

func (s *Store) scope(ctx context.Context) (Scope, error) {
	sc, err := s.kind.Scope(ctx)
	if err == nil {
		return sc, nil
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return Scope{}, err
	}
	return Scope{}, apierrors.NewForbidden(s.kind.DefaultQualifiedResource, "", err)
}

func (s *Store) table() string { return pgx.Identifier{s.kind.Table}.Sanitize() }

func (s *Store) decode(rv int64, data []byte) (runtime.Object, error) {
	obj := s.kind.NewFunc()
	if err := json.Unmarshal(data, obj); err != nil {
		return nil, fmt.Errorf("decode %s: %w", s.kind.DefaultQualifiedResource, err)
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	m.SetResourceVersion(strconv.FormatInt(rv, 10))
	return obj, nil
}

// encode returns the stored form: the object without its resourceVersion,
// which lives in its own column.
func encode(obj runtime.Object) ([]byte, error) {
	c := obj.DeepCopyObject()
	m, err := meta.Accessor(c)
	if err != nil {
		return nil, err
	}
	m.SetResourceVersion("")
	return json.Marshal(c)
}

// current is a loaded row.
type current struct {
	scope Scope
	rv    int64
	obj   runtime.Object
}

// load reads one object. In AllScopes, a name that exists for more than one
// owner is ambiguous and rejected.
func (s *Store) load(ctx context.Context, sc Scope, name string, forUpdate bool) (*current, error) {
	w := &where{}
	w.scope(sc)
	s.kind.readFilter(ctx, w)
	w.add("name = %s", name)
	q := fmt.Sprintf("SELECT scope_type, scope_name, resource_version, object FROM %s WHERE %s LIMIT 2", s.table(), w.sql())
	if forUpdate {
		q += " FOR UPDATE"
	}
	rows, err := s.db.Q(ctx).Query(ctx, q, w.args...)
	if err != nil {
		return nil, s.dbErr(err, name)
	}
	defer rows.Close()

	var found []*current
	for rows.Next() {
		var c current
		var data []byte
		if err := rows.Scan(&c.scope.Type, &c.scope.Name, &c.rv, &data); err != nil {
			return nil, err
		}
		if c.obj, err = s.decode(c.rv, data); err != nil {
			return nil, err
		}
		found = append(found, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, s.dbErr(err, name)
	}
	switch len(found) {
	case 0:
		return nil, apierrors.NewNotFound(s.kind.DefaultQualifiedResource, name)
	case 1:
		return found[0], nil
	default:
		return nil, apierrors.NewBadRequest(fmt.Sprintf(
			"%s %q exists for more than one owner; request it from the owner's context", s.kind.DefaultQualifiedResource, name))
	}
}

// Get implements rest.Getter.
func (s *Store) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.load(ctx, sc, name, false)
	if err != nil {
		return nil, err
	}
	if err := s.kind.decorate(ctx, c.obj); err != nil {
		return nil, err
	}
	return c.obj, nil
}

// List implements rest.Lister. Results are ordered by name and paged with
// limit and continue. Pages are not a consistent snapshot: rows written
// between pages can appear or be skipped.
func (s *Store) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	if opts == nil {
		opts = &metainternalversion.ListOptions{}
	}

	w := &where{}
	w.scope(sc)
	s.kind.readFilter(ctx, w)
	w.labelSelector(opts.LabelSelector)
	if err := w.fieldSelector(opts.FieldSelector, s.kind.FieldColumns); err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if opts.Continue != "" {
		c, err := decodeContinue(opts.Continue)
		if err != nil {
			return nil, apierrors.NewBadRequest(err.Error())
		}
		w.after(c)
	}

	q := fmt.Sprintf("SELECT scope_type, scope_name, name, resource_version, object FROM %s WHERE %s ORDER BY name, scope_type, scope_name",
		s.table(), w.sql())
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit+1)
	}

	// Read the list resourceVersion first so it is never newer than a row
	// the page leaves out.
	var listRV int64
	if err := s.db.Q(ctx).QueryRow(ctx, "SELECT last_value FROM resource_version_seq").Scan(&listRV); err != nil {
		return nil, s.dbErr(err, "")
	}

	rows, err := s.db.Q(ctx).Query(ctx, q, w.args...)
	if err != nil {
		return nil, s.dbErr(err, "")
	}
	defer rows.Close()

	var (
		items   []runtime.Object
		scanned int64
		last    continueToken
		more    bool
	)
	for rows.Next() {
		if opts.Limit > 0 && scanned == opts.Limit {
			more = true
			break
		}
		scanned++
		var (
			tok  continueToken
			rv   int64
			data []byte
		)
		if err := rows.Scan(&tok.ScopeType, &tok.ScopeName, &tok.Name, &rv, &data); err != nil {
			return nil, err
		}
		last = tok
		obj, err := s.decode(rv, data)
		if err != nil {
			return nil, err
		}
		// Apply the full label selector, for operators SQL skipped.
		if opts.LabelSelector != nil && !opts.LabelSelector.Empty() {
			m, _ := meta.Accessor(obj)
			if !opts.LabelSelector.Matches(labels.Set(m.GetLabels())) {
				continue
			}
		}
		items = append(items, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, s.dbErr(err, "")
	}

	if err := s.kind.decorate(ctx, items...); err != nil {
		return nil, err
	}
	list := s.kind.NewListFunc()
	if err := meta.SetList(list, items); err != nil {
		return nil, err
	}
	lm, err := meta.ListAccessor(list)
	if err != nil {
		return nil, err
	}
	lm.SetResourceVersion(strconv.FormatInt(listRV, 10))
	if more {
		lm.SetContinue(last.encode())
	}
	return list, nil
}

// Create implements rest.Creater.
func (s *Store) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, opts *metav1.CreateOptions) (runtime.Object, error) {
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	var out runtime.Object
	err = s.db.InTx(ctx, func(ctx context.Context) error {
		var err error
		if out, err = s.create(ctx, sc, obj, createValidation, false); err != nil {
			return err
		}
		if err := s.kind.decorate(ctx, out); err != nil {
			return err
		}
		if opts != nil && len(opts.DryRun) > 0 {
			return errRollback
		}
		return nil
	})
	return out, err
}

func (s *Store) create(ctx context.Context, sc Scope, obj runtime.Object, createValidation rest.ValidateObjectFunc, viaUpdate bool) (runtime.Object, error) {
	if sc.All() {
		return nil, apierrors.NewForbidden(s.kind.DefaultQualifiedResource, "",
			errors.New("create this resource from the owning user's context"))
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	// Same order as the generic registry: system fields and the generated
	// name come before the strategy runs. A create through update or apply
	// may carry stale system fields, so they are wiped first.
	if viaUpdate {
		rest.WipeObjectMetaSystemFields(m)
	}
	rest.FillObjectMetaSystemFields(m)
	if m.GetGenerateName() != "" && m.GetName() == "" {
		m.SetName(s.kind.CreateStrategy.GenerateName(m.GetGenerateName()))
	}

	if err := rest.BeforeCreate(s.kind.CreateStrategy, ctx, obj); err != nil {
		return nil, err
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj.DeepCopyObject()); err != nil {
			return nil, err
		}
	}
	if err := s.kind.beforeWrite(ctx, OpCreate, nil, obj); err != nil {
		return nil, err
	}

	data, err := encode(obj)
	if err != nil {
		return nil, err
	}

	cols := []string{"scope_type", "scope_name", "name", "uid", "created_at", "labels", "object"}
	vals := []any{sc.Type, sc.Name, m.GetName(), string(m.GetUID()), creationTime(m), labelsOf(m), json.RawMessage(data)}
	for _, c := range s.kind.columns(obj) {
		cols = append(cols, c.Name)
		vals = append(vals, c.Value)
	}
	quoted := make([]string, len(cols))
	ph := make([]string, len(vals))
	for i := range cols {
		quoted[i] = pgx.Identifier{cols[i]}.Sanitize()
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s, resource_version) VALUES (%s, nextval('resource_version_seq')) RETURNING resource_version",
		s.table(), strings.Join(quoted, ", "), strings.Join(ph, ", "))

	var rv int64
	if err := s.db.Q(ctx).QueryRow(ctx, q, vals...).Scan(&rv); err != nil {
		return nil, s.dbErr(err, m.GetName())
	}
	m.SetResourceVersion(strconv.FormatInt(rv, 10))
	if err := s.kind.afterWrite(ctx, OpCreate, nil, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// Update implements rest.Updater. It locks the row, applies objInfo to the
// current object, checks the resourceVersion, and writes only if the stored
// form changed.
func (s *Store) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, opts *metav1.UpdateOptions) (runtime.Object, bool, error) {
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, false, err
	}

	var (
		out     runtime.Object
		created bool
	)
	err = s.db.InTx(ctx, func(ctx context.Context) error {
		cur, err := s.load(ctx, sc, name, true)
		switch {
		case apierrors.IsNotFound(err):
			if !forceAllowCreate && !s.kind.UpdateStrategy.AllowCreateOnUpdate(ctx) {
				return err
			}
			obj, err := objInfo.UpdatedObject(ctx, nil)
			if err != nil {
				return err
			}
			if out, err = s.create(ctx, sc, obj, createValidation, true); err != nil {
				return err
			}
			created = true
		case err != nil:
			return err
		default:
			if out, err = s.update(ctx, cur, objInfo, updateValidation); err != nil {
				return err
			}
		}
		if err := s.kind.decorate(ctx, out); err != nil {
			return err
		}
		if opts != nil && len(opts.DryRun) > 0 {
			return errRollback
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

func (s *Store) update(ctx context.Context, cur *current, objInfo rest.UpdatedObjectInfo, updateValidation rest.ValidateObjectUpdateFunc) (runtime.Object, error) {
	old := cur.obj
	obj, err := objInfo.UpdatedObject(ctx, old.DeepCopyObject())
	if err != nil {
		return nil, err
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	curRV := strconv.FormatInt(cur.rv, 10)
	switch rv := m.GetResourceVersion(); {
	case rv == "" && !s.kind.UpdateStrategy.AllowUnconditionalUpdate(ctx):
		return nil, apierrors.NewBadRequest("metadata.resourceVersion must be set for an update")
	case rv == "":
		m.SetResourceVersion(curRV)
	case rv != curRV:
		return nil, apierrors.NewConflict(s.kind.DefaultQualifiedResource, m.GetName(),
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}

	if err := rest.BeforeUpdate(s.kind.UpdateStrategy, ctx, obj, old); err != nil {
		return nil, err
	}
	if updateValidation != nil {
		if err := updateValidation(ctx, obj.DeepCopyObject(), old.DeepCopyObject()); err != nil {
			return nil, err
		}
	}
	if err := s.kind.beforeWrite(ctx, OpUpdate, old, obj); err != nil {
		return nil, err
	}

	data, err := encode(obj)
	if err != nil {
		return nil, err
	}
	oldData, err := encode(old)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(data, oldData) {
		return old, nil
	}

	w := &where{}
	sets := []string{
		"resource_version = nextval('resource_version_seq')",
		"labels = " + w.arg(labelsOf(m)),
		"object = " + w.arg(json.RawMessage(data)),
	}
	for _, c := range s.kind.columns(obj) {
		sets = append(sets, pgx.Identifier{c.Name}.Sanitize()+" = "+w.arg(c.Value))
	}
	w.add("scope_type = %s AND scope_name = %s AND name = %s AND resource_version = %s",
		cur.scope.Type, cur.scope.Name, m.GetName(), cur.rv)
	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s RETURNING resource_version", s.table(), strings.Join(sets, ", "), w.sql())

	var rv int64
	if err := s.db.Q(ctx).QueryRow(ctx, q, w.args...).Scan(&rv); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.NewConflict(s.kind.DefaultQualifiedResource, m.GetName(), errors.New("the object has been modified"))
		}
		return nil, s.dbErr(err, m.GetName())
	}
	m.SetResourceVersion(strconv.FormatInt(rv, 10))
	if err := s.kind.afterWrite(ctx, OpUpdate, old, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// Delete implements rest.GracefulDeleter. Deletion is immediate; finalizers
// and grace periods are not supported.
func (s *Store) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	sc, err := s.scope(ctx)
	if err != nil {
		return nil, false, err
	}
	var out runtime.Object
	err = s.db.InTx(ctx, func(ctx context.Context) error {
		cur, err := s.load(ctx, sc, name, true)
		if err != nil {
			return err
		}
		m, err := meta.Accessor(cur.obj)
		if err != nil {
			return err
		}
		if opts != nil && opts.Preconditions != nil {
			p := opts.Preconditions
			if p.UID != nil && *p.UID != m.GetUID() {
				return apierrors.NewConflict(s.kind.DefaultQualifiedResource, name,
					fmt.Errorf("precondition failed: UID in precondition: %v, UID in object meta: %v", *p.UID, m.GetUID()))
			}
			if p.ResourceVersion != nil && *p.ResourceVersion != m.GetResourceVersion() {
				return apierrors.NewConflict(s.kind.DefaultQualifiedResource, name,
					fmt.Errorf("precondition failed: ResourceVersion in precondition: %v, ResourceVersion in object meta: %v", *p.ResourceVersion, m.GetResourceVersion()))
			}
		}
		if deleteValidation != nil {
			if err := deleteValidation(ctx, cur.obj); err != nil {
				return err
			}
		}
		if err := s.kind.beforeWrite(ctx, OpDelete, cur.obj, nil); err != nil {
			return err
		}
		if _, err := s.db.Q(ctx).Exec(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE scope_type = $1 AND scope_name = $2 AND name = $3", s.table()),
			cur.scope.Type, cur.scope.Name, name); err != nil {
			return s.dbErr(err, name)
		}
		if err := s.kind.afterWrite(ctx, OpDelete, cur.obj, nil); err != nil {
			return err
		}
		out = cur.obj
		if opts != nil && len(opts.DryRun) > 0 {
			return errRollback
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// dbErr maps Postgres errors onto API errors.
func (s *Store) dbErr(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return apierrors.NewAlreadyExists(s.kind.DefaultQualifiedResource, name)
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return err
	}
	return apierrors.NewInternalError(fmt.Errorf("%s storage: %w", s.kind.DefaultQualifiedResource, err))
}

func creationTime(m metav1.Object) time.Time {
	if t := m.GetCreationTimestamp(); !t.IsZero() {
		return t.Time
	}
	return time.Now()
}

func labelsOf(m metav1.Object) map[string]string {
	if l := m.GetLabels(); l != nil {
		return l
	}
	return map[string]string{}
}

// StatusStore serves a status subresource: get, and update with the status
// strategy.
type StatusStore struct {
	store *Store
}

var (
	_ rest.Getter              = (*StatusStore)(nil)
	_ rest.Updater             = (*StatusStore)(nil)
	_ rest.ResetFieldsStrategy = (*StatusStore)(nil)
)

// NewStatusStore returns status subresource storage that updates through
// strategy.
func NewStatusStore(main *Store, strategy rest.RESTUpdateStrategy) *StatusStore {
	k := main.kind
	k.UpdateStrategy = strategy
	return &StatusStore{store: &Store{db: main.db, kind: k}}
}

func (s *StatusStore) New() runtime.Object { return s.store.New() }
func (s *StatusStore) Destroy()            {}

func (s *StatusStore) Get(ctx context.Context, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	return s.store.Get(ctx, name, opts)
}

func (s *StatusStore) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc, _ bool, opts *metav1.UpdateOptions) (runtime.Object, bool, error) {
	// A status update never creates the object.
	return s.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, opts)
}

func (s *StatusStore) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return s.store.GetResetFields()
}
