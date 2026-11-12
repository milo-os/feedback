package securityreport

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// AcknowledgementREST serves SecurityAcknowledgement: a read-only view of
// reports in Fixed or Disclosed whose reporter chose public credit. Each
// holds the credited name, link, month and severity, and nothing else from
// the report.
type AcknowledgementREST struct {
	db    *pg.DB
	table rest.TableConvertor
}

var (
	_ rest.Getter               = &AcknowledgementREST{}
	_ rest.Lister               = &AcknowledgementREST{}
	_ rest.Scoper               = &AcknowledgementREST{}
	_ rest.SingularNameProvider = &AcknowledgementREST{}
)

// NewAcknowledgementREST returns the view.
func NewAcknowledgementREST(db *pg.DB) *AcknowledgementREST {
	return &AcknowledgementREST{db: db, table: ackTable{}}
}

func (r *AcknowledgementREST) New() runtime.Object     { return &v1.SecurityAcknowledgement{} }
func (r *AcknowledgementREST) NewList() runtime.Object { return &v1.SecurityAcknowledgementList{} }
func (r *AcknowledgementREST) Destroy()                {}
func (r *AcknowledgementREST) NamespaceScoped() bool   { return false }
func (r *AcknowledgementREST) GetSingularName() string { return "securityacknowledgement" }
func (r *AcknowledgementREST) ConvertToTable(ctx context.Context, obj, opts runtime.Object) (*metav1.Table, error) {
	return r.table.ConvertToTable(ctx, obj, opts)
}

const ackQuery = `SELECT name, created_at, object->'spec'->'credit', object->'status' FROM security_reports
	WHERE phase IN ('Fixed', 'Disclosed')
	  AND coalesce((object->'spec'->'credit'->>'public')::boolean, false)
	  AND coalesce(object->'spec'->'credit'->>'name', '') <> ''`

// Get returns one acknowledgement, named after its report.
func (r *AcknowledgementREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	acks, err := r.query(ctx, ackQuery+" AND name = $1", name)
	if err != nil {
		return nil, err
	}
	if len(acks) == 0 {
		return nil, apierrors.NewNotFound(v1.Resource("securityacknowledgements"), name)
	}
	return &acks[0], nil
}

// List returns every acknowledgement, newest month first. The list is short,
// so it isn't paged; selectors aren't supported.
func (r *AcknowledgementREST) List(ctx context.Context, opts *metainternalversion.ListOptions) (runtime.Object, error) {
	if opts != nil && ((opts.LabelSelector != nil && !opts.LabelSelector.Empty()) ||
		(opts.FieldSelector != nil && !opts.FieldSelector.Empty() && !isNameSelector(opts))) {
		return nil, apierrors.NewBadRequest("securityacknowledgements support no selectors except metadata.name")
	}
	q, args := ackQuery, []any{}
	if opts != nil && isNameSelector(opts) {
		v, _ := opts.FieldSelector.RequiresExactMatch("metadata.name")
		q, args = q+" AND name = $1", []any{v}
	}
	acks, err := r.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(acks, func(a, b v1.SecurityAcknowledgement) int {
		if c := strings.Compare(b.Status.Month, a.Status.Month); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return &v1.SecurityAcknowledgementList{Items: acks}, nil
}

func isNameSelector(opts *metainternalversion.ListOptions) bool {
	if opts.FieldSelector == nil {
		return false
	}
	_, ok := opts.FieldSelector.RequiresExactMatch("metadata.name")
	return ok && len(opts.FieldSelector.Requirements()) == 1
}

func (r *AcknowledgementREST) query(ctx context.Context, q string, args ...any) ([]v1.SecurityAcknowledgement, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	acks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (v1.SecurityAcknowledgement, error) {
		var (
			name           string
			created        time.Time
			rawCredit, raw []byte
			credit         v1.SecurityCredit
			st             v1.SecurityReportStatus
		)
		if err := row.Scan(&name, &created, &rawCredit, &raw); err != nil {
			return v1.SecurityAcknowledgement{}, err
		}
		if err := json.Unmarshal(rawCredit, &credit); err != nil {
			return v1.SecurityAcknowledgement{}, fmt.Errorf("decode credit of %s: %w", name, err)
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return v1.SecurityAcknowledgement{}, fmt.Errorf("decode status of %s: %w", name, err)
		}
		return v1.SecurityAcknowledgement{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: v1.SecurityAcknowledgementStatus{
				Name: credit.Name, URL: credit.URL, Month: fixedMonth(st, created), Severity: st.Severity,
			},
		}, nil
	})
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	return acks, nil
}

// fixedMonth is when the report was fixed: its first Fixed timeline entry,
// else its first Disclosed one, else when it was received.
func fixedMonth(st v1.SecurityReportStatus, received time.Time) string {
	for _, phase := range []v1.SecurityReportPhase{v1.SecurityReportPhaseFixed, v1.SecurityReportPhaseDisclosed} {
		for _, e := range st.Timeline {
			if e.Phase == phase {
				return e.At.UTC().Format("2006-01")
			}
		}
	}
	return received.UTC().Format("2006-01")
}

type ackTable struct{}

func (ackTable) ConvertToTable(_ context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name"},
		{Name: "Credit", Type: "string"},
		{Name: "Month", Type: "string"},
		{Name: "Severity", Type: "string"},
		{Name: "URL", Type: "string", Priority: 1},
	}}
	add := func(a *v1.SecurityAcknowledgement) {
		t.Rows = append(t.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: a},
			Cells:  []any{a.Name, a.Status.Name, a.Status.Month, string(a.Status.Severity), a.Status.URL},
		})
	}
	switch o := obj.(type) {
	case *v1.SecurityAcknowledgement:
		add(o)
	case *v1.SecurityAcknowledgementList:
		for i := range o.Items {
			add(&o.Items[i])
		}
	}
	return t, nil
}
