package postquery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// candidates is where results come from before filters apply.
type candidates struct {
	// hits, when non-nil, limits results to these posts in this order
	// (keyword search). An empty non-nil slice matches nothing.
	hits []Hit
	// vector, when set, ranks posts by cosine distance from it, keeping the
	// nearest similarCap (similarTo).
	vector     string
	dimensions int
	model      string
}

// query is one PostQuery after validation.
type query struct {
	spec   v1.PostQuerySpec
	sort   v1.PostSort
	limit  int
	offset int
	// user is the caller's UID in the user context, for votedByMe and
	// followedByMe; empty elsewhere.
	user string
	from candidates
}

// similarCap is how many nearest posts similarTo considers. Sorting
// similar posts by anything but relevance only reorders these.
const similarCap = 100

// facetColumns maps facet names to the values they count.
var facetColumns = map[string]string{
	"state":         "p.state",
	"deliveryStage": "p.delivery_stage",
	"orgs":          "lower(p.github_org)",
	"categories":    "jsonb_array_elements_text(coalesce(p.object->'status'->'categories', '[]'))",
}

type sqlArgs []any

func (a *sqlArgs) add(v any) string {
	*a = append(*a, v)
	return fmt.Sprintf("$%d", len(*a))
}

// matched builds a CTE body selecting (name, score, rank) for every post
// that matches; lower rank is more relevant.
func (q *query) matched(args *sqlArgs) string {
	conds := []string{"p.scope_type = " + args.add(pg.ScopePlatform), "NOT p.hidden"}
	f := q.spec.Filter
	if len(f.State) > 0 {
		conds = append(conds, "p.state = ANY("+args.add(strs(f.State))+"::text[])")
	}
	if len(f.DeliveryStage) > 0 {
		conds = append(conds, "p.delivery_stage = ANY("+args.add(strs(f.DeliveryStage))+"::text[])")
	}
	if len(f.Orgs) > 0 {
		orgs := make([]string, len(f.Orgs))
		for i, o := range f.Orgs {
			orgs[i] = strings.ToLower(o)
		}
		conds = append(conds, "lower(p.github_org) = ANY("+args.add(orgs)+"::text[])")
	}
	if len(f.Categories) > 0 {
		conds = append(conds, "coalesce(p.object->'status'->'categories', '[]') ?| "+args.add(f.Categories)+"::text[]")
	}
	if f.VotedByMe {
		conds = append(conds, q.mine("votes", args))
	}
	if f.FollowedByMe {
		conds = append(conds, q.mine("subscriptions", args))
	}
	where := strings.Join(conds, " AND ")

	switch {
	case q.from.vector != "":
		dist := "(e.embedding <=> " + args.add(q.from.vector) + "::vector)"
		return fmt.Sprintf(`SELECT p.name, (1 - %[1]s)::float8 AS score, %[1]s::float8 AS rank
			FROM posts p JOIN post_embeddings e ON e.post_name = p.name
			WHERE %[2]s AND e.model = %[3]s AND vector_dims(e.embedding) = %[4]s
			ORDER BY rank LIMIT %[5]d`,
			dist, where, args.add(q.from.model), args.add(q.from.dimensions), similarCap)
	case q.from.hits != nil:
		names := make([]string, len(q.from.hits))
		scores := make([]float64, len(q.from.hits))
		for i, h := range q.from.hits {
			names[i], scores[i] = h.Name, h.Score
		}
		return fmt.Sprintf(`SELECT p.name, h.score, h.ord::float8 AS rank
			FROM posts p
			JOIN unnest(%s::text[], %s::float8[]) WITH ORDINALITY AS h(name, score, ord) ON h.name = p.name
			WHERE %s`, args.add(names), args.add(scores), where)
	default:
		return "SELECT p.name, NULL::float8 AS score, NULL::float8 AS rank FROM posts p WHERE " + where
	}
}

// mine matches posts the caller has a row for in table (votes or
// subscriptions).
func (q *query) mine(table string, args *sqlArgs) string {
	return fmt.Sprintf("EXISTS (SELECT 1 FROM %s x WHERE x.scope_type = %s AND x.scope_name = %s AND x.post_name = p.name)",
		table, args.add(pg.ScopeUser), args.add(q.user))
}

func (q *query) orderBy() string {
	switch q.sort {
	case v1.PostSortRelevance:
		return "m.rank, p.name"
	case v1.PostSortNewest:
		return "p.github_created_at DESC NULLS LAST, p.name"
	case v1.PostSortTrending:
		return "p.trending_score DESC, p.name"
	case v1.PostSortRecentlyChanged:
		return "p.delivery_changed_at DESC NULLS LAST, p.name"
	default:
		return "p.votes_total DESC, p.name"
	}
}

// results returns one page, and whether there is another.
func (q *query) results(ctx context.Context, db pg.Querier) ([]v1.PostQueryResult, bool, error) {
	var args sqlArgs
	matched := q.matched(&args)
	voted := "false"
	if q.user != "" {
		voted = q.mine("votes", &args)
	}
	sql := fmt.Sprintf(`WITH m AS (%s)
		SELECT p.name, coalesce(p.object->'status', '{}'), m.score, %s
		FROM m JOIN posts p ON p.scope_type = %s AND p.name = m.name
		ORDER BY %s
		LIMIT %d OFFSET %d`,
		matched, voted, args.add(pg.ScopePlatform), q.orderBy(), q.limit+1, q.offset)
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []v1.PostQueryResult
	for rows.Next() {
		var (
			r      v1.PostQueryResult
			status []byte
			st     v1.PostStatus
		)
		if err := rows.Scan(&r.Name, &status, &r.Score, &r.VotedByMe); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(status, &st); err != nil {
			return nil, false, fmt.Errorf("decode post %s: %w", r.Name, err)
		}
		r.Title, r.State, r.Categories, r.Votes = st.Title, st.State, st.Categories, st.Votes
		if st.Delivery != nil {
			r.DeliveryStage = st.Delivery.Stage
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > q.limit
	if more {
		out = out[:q.limit]
	}
	return out, more, nil
}

// facets counts values of each requested facet over every match, not just
// this page.
func (q *query) facets(ctx context.Context, db pg.Querier) (map[string]map[string]int32, error) {
	if len(q.spec.Facets) == 0 {
		return nil, nil
	}
	var args sqlArgs
	matched := q.matched(&args)
	platform := args.add(pg.ScopePlatform)
	parts := make([]string, len(q.spec.Facets))
	for i, f := range q.spec.Facets {
		parts[i] = fmt.Sprintf("SELECT %s::text AS facet, %s AS value FROM m JOIN posts p ON p.scope_type = %s AND p.name = m.name",
			args.add(f), facetColumns[f], platform)
	}
	sql := fmt.Sprintf(`WITH m AS (%s)
		SELECT facet, value, count(*)::int4 FROM (%s) x
		WHERE value <> '' GROUP BY facet, value`, matched, strings.Join(parts, " UNION ALL "))
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]int32, len(q.spec.Facets))
	for _, f := range q.spec.Facets {
		out[f] = map[string]int32{}
	}
	for rows.Next() {
		var facet, value string
		var n int32
		if err := rows.Scan(&facet, &value, &n); err != nil {
			return nil, err
		}
		out[facet][value] = n
	}
	return out, rows.Err()
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}
