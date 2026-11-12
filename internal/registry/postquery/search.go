package postquery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// Hit is one matching post, best first.
type Hit struct {
	Name  string
	Score float64
}

// Searcher finds posts matching keywords, best first, at most max of them.
type Searcher interface {
	Search(ctx context.Context, text string, max int) ([]Hit, error)
}

// Keyword matches posts in Postgres with full-text search over title,
// summary and body. A post matches if it has any of the words; posts with
// more of them, and with them in the title, rank higher.
type Keyword struct {
	DB *pg.DB
}

// Search implements Searcher.
func (k Keyword) Search(ctx context.Context, text string, max int) ([]Hit, error) {
	// plainto_tsquery ANDs the words; swapping & for | makes any word match.
	// The cast doesn't re-stem the lexemes plainto_tsquery produced.
	rows, err := k.DB.Q(ctx).Query(ctx, `
		SELECT p.name, ts_rank_cd(p.search_text, q, 32)::float8
		FROM posts p
		CROSS JOIN (SELECT replace(plainto_tsquery('english', $1)::text, '&', '|')::tsquery AS q) t
		WHERE p.scope_type = $2 AND NOT p.hidden AND p.search_text @@ q
		ORDER BY 2 DESC, p.name
		LIMIT $3`, text, pg.ScopePlatform, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Name, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// SearchService runs ResourceSearchQuery against the posts index in Milo's
// search service, with the apiserver's own credentials.
type SearchService struct {
	url    string
	client *http.Client
}

// searchPageSize is the search service's maximum page size.
const searchPageSize = 100

// NewSearchServiceForConfig returns a client for the API server in cfg.
func NewSearchServiceForConfig(cfg *rest.Config) (*SearchService, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &SearchService{
		url:    strings.TrimSuffix(cfg.Host, "/") + "/apis/search.miloapis.com/v1alpha1/resourcesearchqueries",
		client: hc,
	}, nil
}

type searchQuery struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       searchSpec        `json:"spec"`
}

type searchSpec struct {
	Query           string         `json:"query"`
	Limit           int32          `json:"limit"`
	Continue        string         `json:"continue,omitempty"`
	TargetResources []searchTarget `json:"targetResources"`
}

type searchTarget struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type searchResult struct {
	Status struct {
		Results []struct {
			Resource struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"resource"`
			RelevanceScore float64 `json:"relevanceScore"`
		} `json:"results"`
		DeniedTargetResources []searchTarget `json:"deniedTargetResources"`
		Continue              string         `json:"continue"`
	} `json:"status"`
}

// Search implements Searcher, paging through results until it has max.
func (s *SearchService) Search(ctx context.Context, text string, max int) ([]Hit, error) {
	var hits []Hit
	cont := ""
	for len(hits) < max {
		page, next, err := s.page(ctx, text, cont)
		if err != nil {
			return nil, err
		}
		hits = append(hits, page...)
		if next == "" || len(page) == 0 {
			break
		}
		cont = next
	}
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits, nil
}

func (s *SearchService) page(ctx context.Context, text, cont string) ([]Hit, string, error) {
	body, err := json.Marshal(searchQuery{
		APIVersion: "search.miloapis.com/v1alpha1",
		Kind:       "ResourceSearchQuery",
		Metadata:   metav1.ObjectMeta{Name: "feedback-postquery"},
		Spec: searchSpec{
			Query:    text,
			Limit:    searchPageSize,
			Continue: cont,
			TargetResources: []searchTarget{{
				Group: v1.GroupName, Version: v1.SchemeGroupVersion.Version, Kind: "Post",
			}},
		},
	})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var st metav1.Status
		if json.Unmarshal(msg, &st) == nil && st.Message != "" {
			return nil, "", fmt.Errorf("search service: %s", st.Message)
		}
		return nil, "", fmt.Errorf("search service returned %s", resp.Status)
	}
	var out searchResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("decode search response: %w", err)
	}
	if len(out.Status.DeniedTargetResources) > 0 {
		return nil, "", fmt.Errorf("search service has no index policy for posts")
	}
	hits := make([]Hit, 0, len(out.Status.Results))
	for _, r := range out.Status.Results {
		if n := r.Resource.Metadata.Name; n != "" {
			hits = append(hits, Hit{Name: n, Score: r.RelevanceScore})
		}
	}
	return hits, out.Status.Continue, nil
}
