// Package postquery serves PostQuery: a create-to-query resource that
// searches, filters, sorts and facets posts. Nothing is stored.
package postquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/embedding"
	"go.miloapis.com/feedback/internal/ratelimit"
	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	maxTextLen   = 500
	// maxHits caps keyword matches considered, before filters.
	maxHits = 500
)

// Options configures where PostQuery gets matches from.
type Options struct {
	// Search answers spec.text. Nil uses Postgres keyword matching.
	Search Searcher
	// Embedder answers spec.similarTo. Nil uses keyword matching.
	Embedder embedding.Embedder
	// Dimensions is the length of the embedder's vectors, so vectors from an
	// earlier setting are skipped. Zero accepts whatever the embedder returns.
	Dimensions int
	// SimilarLimit rate limits similarTo per user. Nil doesn't limit.
	SimilarLimit *ratelimit.Limiter
}

// REST serves postqueries.
type REST struct {
	db      *pg.DB
	keyword Keyword
	opts    Options
	table   rest.TableConvertor
}

var (
	_ rest.Storage              = &REST{}
	_ rest.Creater              = &REST{}
	_ rest.Scoper               = &REST{}
	_ rest.SingularNameProvider = &REST{}
	_ rest.TableConvertor       = &REST{}
)

// NewREST returns PostQuery storage.
func NewREST(db *pg.DB, opts Options) *REST {
	return &REST{
		db:      db,
		keyword: Keyword{DB: db},
		opts:    opts,
		table:   rest.NewDefaultTableConvertor(v1.Resource("postqueries")),
	}
}

// New implements rest.Storage.
func (r *REST) New() runtime.Object { return &v1.PostQuery{} }

// Destroy implements rest.Storage.
func (r *REST) Destroy() {}

// NamespaceScoped implements rest.Scoper.
func (r *REST) NamespaceScoped() bool { return false }

// GetSingularName implements rest.SingularNameProvider.
func (r *REST) GetSingularName() string { return "postquery" }

// ConvertToTable implements rest.TableConvertor.
func (r *REST) ConvertToTable(ctx context.Context, obj, opts runtime.Object) (*metav1.Table, error) {
	return r.table.ConvertToTable(ctx, obj, opts)
}

// Create runs the query and returns it with status filled in.
func (r *REST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	pq, ok := obj.(*v1.PostQuery)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("not a PostQuery: %T", obj))
	}
	if createValidation != nil {
		if err := createValidation(ctx, obj); err != nil {
			return nil, err
		}
	}

	kind, name := pg.RequestContext(ctx)
	user := ""
	if kind == pg.ScopeUser {
		user = name
	}
	q, errs := parse(pq.Spec, user)
	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(v1.Kind("PostQuery"), pq.Name, errs)
	}

	switch {
	case q.spec.Text != "":
		src := r.opts.Search
		if src == nil {
			src = r.keyword
		}
		hits, err := src.Search(ctx, q.spec.Text, maxHits)
		if err != nil {
			klog.ErrorS(err, "PostQuery text search failed")
			return nil, apierrors.NewServiceUnavailable("Keyword search is unavailable. Browsing, filters and similarTo still work.")
		}
		q.from.hits = nonNil(hits)
	case q.spec.SimilarTo != "":
		if ok, wait := r.opts.SimilarLimit.Allow(callerKey(ctx)); !ok {
			return nil, apierrors.NewTooManyRequests("Too many similarTo queries; try again shortly.", int(math.Ceil(wait.Seconds())))
		}
		if err := r.similar(ctx, &q); err != nil {
			return nil, apierrors.NewServiceUnavailable("Similarity search is unavailable.")
		}
	}

	var (
		results []v1.PostQueryResult
		more    bool
		facets  map[string]map[string]int32
	)
	err := r.db.InTx(ctx, func(ctx context.Context) error {
		var err error
		if results, more, err = q.results(ctx, r.db.Q(ctx)); err != nil {
			return err
		}
		facets, err = q.facets(ctx, r.db.Q(ctx))
		return err
	})
	if err != nil {
		klog.ErrorS(err, "PostQuery failed")
		return nil, apierrors.NewInternalError(fmt.Errorf("query posts"))
	}

	out := pq.DeepCopy()
	out.CreationTimestamp = metav1.NewTime(time.Now())
	out.Status = v1.PostQueryStatus{Results: results, Facets: facets}
	if more {
		out.Status.Continue = encodeContinue(q.spec, q.offset+len(results))
	}
	return out, nil
}

// similar sets q's candidates from spec.similarTo: nearest posts by
// embedding, or keyword matches when embeddings aren't available.
func (r *REST) similar(ctx context.Context, q *query) error {
	text := embedding.Truncate(q.spec.SimilarTo)
	if e := r.opts.Embedder; e != nil {
		var has bool
		err := r.db.Q(ctx).QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM post_embeddings WHERE model = $1)", e.Model()).Scan(&has)
		if err != nil {
			return err
		}
		if has {
			vecs, err := e.Embed(ctx, []string{text})
			if err == nil {
				q.from.vector = embedding.Vector(vecs[0])
				q.from.model = e.Model()
				q.from.dimensions = len(vecs[0])
				if r.opts.Dimensions > 0 && q.from.dimensions != r.opts.Dimensions {
					klog.InfoS("Embedding length differs from --embedding-dimensions", "got", q.from.dimensions, "want", r.opts.Dimensions)
				}
				return nil
			}
			klog.ErrorS(err, "Embedding similarTo text failed; falling back to keyword matching")
		}
	}
	warning.AddWarning(ctx, "", "similarTo used keyword matching because embeddings are unavailable")
	hits, err := r.keyword.Search(ctx, text, maxHits)
	if err != nil {
		return err
	}
	q.from.hits = nonNil(hits)
	return nil
}

// callerKey identifies the caller for rate limiting.
func callerKey(ctx context.Context) string {
	if u, ok := request.UserFrom(ctx); ok {
		if uid := u.GetUID(); uid != "" {
			return uid
		}
		return u.GetName()
	}
	return ""
}

func nonNil(h []Hit) []Hit {
	if h == nil {
		return []Hit{}
	}
	return h
}

// parse validates spec and fills in defaults.
func parse(spec v1.PostQuerySpec, user string) (query, field.ErrorList) {
	p := field.NewPath("spec")
	var errs field.ErrorList
	spec.Text = strings.TrimSpace(spec.Text)
	spec.SimilarTo = strings.TrimSpace(spec.SimilarTo)

	if len(spec.Text) > maxTextLen {
		errs = append(errs, field.TooLong(p.Child("text"), "", maxTextLen))
	}
	if len(spec.SimilarTo) > embedding.MaxInputChars {
		errs = append(errs, field.TooLong(p.Child("similarTo"), "", embedding.MaxInputChars))
	}
	if spec.Text != "" && spec.SimilarTo != "" {
		errs = append(errs, field.Invalid(p.Child("similarTo"), "", "set text or similarTo, not both"))
	}

	sort := spec.Sort
	switch {
	case sort == "" && (spec.Text != "" || spec.SimilarTo != ""):
		sort = v1.PostSortRelevance
	case sort == "":
		sort = v1.PostSortTop
	case !slices.Contains(v1.ValidPostSorts, sort):
		errs = append(errs, field.NotSupported(p.Child("sort"), sort, v1.ValidPostSorts))
	case sort == v1.PostSortRelevance && spec.Text == "" && spec.SimilarTo == "":
		errs = append(errs, field.Invalid(p.Child("sort"), sort, "relevance needs text or similarTo"))
	}

	limit := int(spec.Limit)
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0 || limit > maxLimit:
		errs = append(errs, field.Invalid(p.Child("limit"), spec.Limit, fmt.Sprintf("must be between 1 and %d", maxLimit)))
	}

	fp := p.Child("filter")
	for i, s := range spec.Filter.State {
		if !slices.Contains(v1.ValidPostStates, s) {
			errs = append(errs, field.NotSupported(fp.Child("state").Index(i), s, v1.ValidPostStates))
		}
	}
	for i, s := range spec.Filter.DeliveryStage {
		if !slices.Contains(v1.ValidDeliveryStages, s) {
			errs = append(errs, field.NotSupported(fp.Child("deliveryStage").Index(i), s, v1.ValidDeliveryStages))
		}
	}
	if user == "" {
		if spec.Filter.VotedByMe {
			errs = append(errs, field.Forbidden(fp.Child("votedByMe"), "only available in the user context"))
		}
		if spec.Filter.FollowedByMe {
			errs = append(errs, field.Forbidden(fp.Child("followedByMe"), "only available in the user context"))
		}
	}

	seen := map[string]bool{}
	for i, f := range spec.Facets {
		switch {
		case facetColumns[f] == "":
			errs = append(errs, field.NotSupported(p.Child("facets").Index(i), f, []string{"state", "categories", "orgs", "deliveryStage"}))
		case seen[f]:
			errs = append(errs, field.Duplicate(p.Child("facets").Index(i), f))
		}
		seen[f] = true
	}

	offset := 0
	if spec.Continue != "" {
		var err error
		if offset, err = decodeContinue(spec); err != nil {
			errs = append(errs, field.Invalid(p.Child("continue"), spec.Continue, err.Error()))
		}
	}
	return query{spec: spec, sort: sort, limit: limit, offset: offset, user: user}, errs
}

// A continue token is the offset of the next page plus a hash of the query,
// so a token can't be reused with a different query.
type continueToken struct {
	Offset int    `json:"o"`
	Query  uint64 `json:"q"`
}

func queryHash(spec v1.PostQuerySpec) uint64 {
	spec.Continue, spec.Limit = "", 0
	b, _ := json.Marshal(spec)
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

func encodeContinue(spec v1.PostQuerySpec, offset int) string {
	b, _ := json.Marshal(continueToken{Offset: offset, Query: queryHash(spec)})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeContinue(spec v1.PostQuerySpec) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(spec.Continue)
	var t continueToken
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	if err != nil || t.Offset < 0 {
		return 0, fmt.Errorf("not a continue token from this API")
	}
	if t.Query != queryHash(spec) {
		return 0, fmt.Errorf("the query changed; start again without continue")
	}
	return t.Offset, nil
}
