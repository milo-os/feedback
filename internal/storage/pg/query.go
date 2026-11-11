package pg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// where accumulates SQL conditions and their positional arguments.
type where struct {
	conds []string
	args  []any
}

func (w *where) arg(v any) string {
	w.args = append(w.args, v)
	return fmt.Sprintf("$%d", len(w.args))
}

func (w *where) add(format string, vals ...any) {
	ph := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = w.arg(v)
	}
	w.conds = append(w.conds, fmt.Sprintf(format, ph...))
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(w.conds, " AND ")
}

func (w *where) scope(s Scope) {
	if s.All() {
		return
	}
	w.add("scope_type = %s AND scope_name = %s", s.Type, s.Name)
}

// labelSelector translates what it can of sel into SQL. The caller still
// checks every row with sel.Matches, so operators it skips (Gt, Lt) are
// applied in Go.
func (w *where) labelSelector(sel labels.Selector) {
	if sel == nil || sel.Empty() {
		return
	}
	reqs, _ := sel.Requirements()
	for _, r := range reqs {
		key, vals := r.Key(), r.Values().List()
		switch r.Operator() {
		case selection.Equals, selection.DoubleEquals:
			w.add("labels->>%s = %s", key, vals[0])
		case selection.In:
			w.add("labels->>%s = ANY(%s)", key, vals)
		case selection.NotEquals:
			w.add("labels->>%s IS DISTINCT FROM %s", key, vals[0])
		case selection.NotIn:
			w.add("COALESCE(labels->>%s = ANY(%s), false) = false", key, vals)
		case selection.Exists:
			w.add("labels ? %s", key)
		case selection.DoesNotExist:
			w.add("NOT (labels ? %s)", key)
		}
	}
}

// fieldSelector translates sel using the kind's column map. Unknown fields
// are an error; field selectors only support = and !=.
func (w *where) fieldSelector(sel fields.Selector, columns map[string]string) error {
	if sel == nil || sel.Empty() {
		return nil
	}
	for _, r := range sel.Requirements() {
		col := "name"
		if r.Field != "metadata.name" {
			c, ok := columns[r.Field]
			if !ok {
				return fmt.Errorf("field selector %q is not supported", r.Field)
			}
			col = c
		}
		col = pgx.Identifier{col}.Sanitize()
		switch r.Operator {
		case selection.Equals, selection.DoubleEquals:
			w.add(col+"::text = %s", r.Value)
		case selection.NotEquals:
			w.add(col+"::text <> %s", r.Value)
		default:
			return fmt.Errorf("field selector operator %q is not supported", r.Operator)
		}
	}
	return nil
}

// continueToken is the position after the last row of a page. Rows are
// ordered by (name, scope_type, scope_name), which is unique.
type continueToken struct {
	Name      string `json:"n"`
	ScopeType string `json:"t"`
	ScopeName string `json:"s"`
}

func (c continueToken) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeContinue(s string) (continueToken, error) {
	var c continueToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("invalid continue token")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid continue token")
	}
	return c, nil
}

func (w *where) after(c continueToken) {
	w.add("(name, scope_type, scope_name) > (%s, %s, %s)", c.Name, c.ScopeType, c.ScopeName)
}
