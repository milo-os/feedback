package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Issue is what feedback-sync reads about a GitHub issue.
type Issue struct {
	// ID is the GraphQL node ID.
	ID     string
	Org    string
	Repo   string
	Number int32
	URL    string
	Title  string
	// BodyHTML is the body as GitHub renders it, before sanitising.
	BodyHTML string
	// BodyText is the body as plain text.
	BodyText string
	// State is "open" or "closed".
	State string
	// StateReason is "completed", "not_planned", "duplicate", "reopened" or "".
	StateReason string
	// IssueType is the issue type's name, or "".
	IssueType string
	Labels    []string
	// Visibility is the repository's: "public", "private" or "internal".
	Visibility   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ProjectItems []ProjectItem
}

// ProjectItem is an issue's entry on a GitHub project.
type ProjectItem struct {
	// Owner is the login of the org or user that owns the project.
	Owner  string
	Number int32
	// Status is the name of the selected option in the status field, or ""
	// when none is selected.
	Status string
}

// issueFields reads everything feedback-sync needs. $statusField names the
// project's single-select status field.
const issueFields = `
fragment issueFields on Issue {
  id number url title bodyHTML bodyText state stateReason createdAt updatedAt
  issueType { name }
  repository { name visibility owner { login } }
  labels(first: 100) { totalCount nodes { name } }
  projectItems(first: 20, includeArchived: false) { totalCount nodes { ...projectItemFields } }
}
fragment projectItemFields on ProjectV2Item {
  project { number owner { ... on Organization { login } ... on User { login } } }
  status: fieldValueByName(name: $statusField) {
    ... on ProjectV2ItemFieldSingleSelectValue { name }
  }
}`

type issueNode struct {
	ID          string    `json:"id"`
	Number      int32     `json:"number"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	BodyHTML    string    `json:"bodyHTML"`
	BodyText    string    `json:"bodyText"`
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	IssueType   *struct {
		Name string `json:"name"`
	} `json:"issueType"`
	Repository struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
		Owner      struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Labels struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	ProjectItems projectItems `json:"projectItems"`
}

type projectItems struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		Project struct {
			Number int32 `json:"number"`
			Owner  struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"project"`
		Status *struct {
			Name string `json:"name"`
		} `json:"status"`
	} `json:"nodes"`
}

func (p projectItems) items() []ProjectItem {
	var out []ProjectItem
	for _, n := range p.Nodes {
		if n.Project.Owner.Login == "" {
			// The installation can't read this item's project.
			continue
		}
		it := ProjectItem{Owner: n.Project.Owner.Login, Number: n.Project.Number}
		if n.Status != nil {
			it.Status = n.Status.Name
		}
		out = append(out, it)
	}
	return out
}

// logTruncated logs an issue whose labels or project items didn't all fit in
// one read: feedback-sync maps only the ones it got.
func (n *issueNode) logTruncated(ctx context.Context) {
	logTruncated(ctx, n.URL, "labels", len(n.Labels.Nodes), n.Labels.TotalCount)
	n.ProjectItems.logTruncated(ctx, n.URL)
}

func (p projectItems) logTruncated(ctx context.Context, issue string) {
	logTruncated(ctx, issue, "project items", len(p.Nodes), p.TotalCount)
}

func logTruncated(ctx context.Context, issue, what string, got, total int) {
	if total > got {
		logr.FromContextOrDiscard(ctx).Info("GitHub returned only some of an issue's "+what, "issue", issue, "got", got, "total", total)
	}
}

func (n *issueNode) issue() Issue {
	is := Issue{
		ID:           n.ID,
		Org:          n.Repository.Owner.Login,
		Repo:         n.Repository.Name,
		Number:       n.Number,
		URL:          n.URL,
		Title:        n.Title,
		BodyHTML:     n.BodyHTML,
		BodyText:     n.BodyText,
		State:        strings.ToLower(n.State),
		StateReason:  strings.ToLower(n.StateReason),
		Visibility:   strings.ToLower(n.Repository.Visibility),
		CreatedAt:    n.CreatedAt,
		UpdatedAt:    n.UpdatedAt,
		ProjectItems: n.ProjectItems.items(),
	}
	if n.IssueType != nil {
		is.IssueType = n.IssueType.Name
	}
	for _, l := range n.Labels.Nodes {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

// DefaultStatusField is the project field read when a source names none.
const DefaultStatusField = "Status"

func statusFieldOrDefault(f string) string {
	if f == "" {
		return DefaultStatusField
	}
	return f
}

// Issue reads one issue. It returns ErrNotFound when the repository or issue
// doesn't exist, was deleted, or the installation can't see it. A transferred
// issue comes back from its new repository, so callers compare Org and Repo.
func (c *Client) Issue(ctx context.Context, owner, repo string, number int32, statusField string) (*Issue, error) {
	const q = `query($owner: String!, $repo: String!, $number: Int!, $statusField: String!) {
  repository(owner: $owner, name: $repo) { issue(number: $number) { ...issueFields } }
}` + issueFields
	var out struct {
		Repository *struct {
			Issue *issueNode `json:"issue"`
		} `json:"repository"`
	}
	err := c.query(ctx, q, map[string]any{
		"owner": owner, "repo": repo, "number": number, "statusField": statusFieldOrDefault(statusField),
	}, &out)
	errs, isErrs := errors.AsType[Errors](err)
	if err != nil && !(isErrs && errs.ignorable()) {
		return nil, err
	}
	if out.Repository == nil || out.Repository.Issue == nil {
		if isErrs && !errs.notFound() {
			return nil, errs
		}
		return nil, ErrNotFound
	}
	out.Repository.Issue.logTruncated(ctx)
	is := out.Repository.Issue.issue()
	return &is, nil
}

// searchLimit is the most results GitHub's search returns for one query.
const searchLimit = 1000

// searchPageSize keeps each page's cost and size down: bodies can be large.
const searchPageSize = 50

// earliest is before any GitHub issue was created.
var earliest = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)

// SearchIssues returns the issues matching an issue search query, such as
// `org:datum-cloud is:issue is:public label:"Build"`, each once. GitHub
// returns at most 1000 results per query, so a query with more is split by
// creation date. Results are sorted by creation so new issues don't shift
// the pages being read.
//
// Search isn't a complete list: its index lags, and an issue that stops
// matching while the pages are read can push another off a page. Callers
// must re-read an issue before deciding it's gone. When GitHub refuses some
// results, SearchIssues returns the rest with an *IncompleteError.
func (c *Client) SearchIssues(ctx context.Context, query, statusField string, now time.Time) ([]Issue, error) {
	query += " sort:created-asc"
	var inc IncompleteError
	issues, err := c.searchAll(ctx, query, statusField, &inc)
	if errors.Is(err, errTooMany) {
		inc = IncompleteError{}
		issues, err = c.searchRange(ctx, query, statusField, earliest, now.Add(24*time.Hour), &inc)
	}
	if err != nil {
		return nil, err
	}
	// Split ranges share their boundary second.
	seen := make(map[string]bool, len(issues))
	issues = slices.DeleteFunc(issues, func(is Issue) bool {
		dup := seen[is.ID]
		seen[is.ID] = true
		return dup
	})
	if inc.Missing > 0 {
		return issues, fmt.Errorf("searching %q: %w", query, &inc)
	}
	return issues, nil
}

var errTooMany = errors.New("more search results than GitHub returns")

func (c *Client) searchRange(ctx context.Context, query, statusField string, from, to time.Time, inc *IncompleteError) ([]Issue, error) {
	q := fmt.Sprintf("%s created:%s..%s", query, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	issues, err := c.searchAll(ctx, q, statusField, inc)
	if !errors.Is(err, errTooMany) {
		return issues, err
	}
	if to.Sub(from) <= time.Minute {
		return nil, fmt.Errorf("searching %q: more than %d issues created in one minute", query, searchLimit)
	}
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
	a, err := c.searchRange(ctx, query, statusField, from, mid, inc)
	if err != nil {
		return nil, err
	}
	// Ranges are inclusive at both ends, so an issue created at mid is in
	// both halves; callers dedupe by ID.
	b, err := c.searchRange(ctx, query, statusField, mid, to, inc)
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

// searchAll pages through one query. It returns errTooMany, without paging,
// when the query has more results than GitHub returns, and counts refused
// results in inc.
func (c *Client) searchAll(ctx context.Context, query, statusField string, inc *IncompleteError) ([]Issue, error) {
	const q = `query($q: String!, $first: Int!, $after: String, $statusField: String!) {
  search(type: ISSUE, query: $q, first: $first, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes { ...issueFields }
  }
}` + issueFields
	var issues []Issue
	var after *string
	for {
		var out struct {
			Search struct {
				IssueCount int `json:"issueCount"`
				PageInfo   struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				// Nodes are pointers: a node that isn't an issue decodes
				// as an empty object, and an inaccessible one as null.
				Nodes []*issueNode `json:"nodes"`
			} `json:"search"`
		}
		err := c.query(ctx, q, map[string]any{
			"q": query, "first": searchPageSize, "after": after, "statusField": statusFieldOrDefault(statusField),
		}, &out)
		errs, isErrs := errors.AsType[Errors](err)
		if isErrs && errs.ignorable() {
			err = nil
		}
		if err != nil {
			return nil, fmt.Errorf("searching %q: %w", query, err)
		}
		if out.Search.IssueCount > searchLimit {
			return nil, errTooMany
		}
		// A null node is one the installation can't see, unless an error
		// points at it: an error on a field such as projectItems nulls the
		// whole issue.
		refused := errs.refused("search", "nodes")
		for i, n := range out.Search.Nodes {
			switch {
			case n == nil && refused[i]:
				inc.add("", errs)
			case n != nil && n.ID != "":
				n.logTruncated(ctx)
				issues = append(issues, n.issue())
			}
		}
		if !out.Search.PageInfo.HasNextPage {
			return issues, nil
		}
		cursor := out.Search.PageInfo.EndCursor
		after = &cursor
	}
}

// ProjectItems reads the project items of issues by node ID, as this
// installation sees them. A project owned by another org is only visible to
// that org's installation, so feedback-sync reads it there. Issues that
// aren't found are left out. When GitHub refuses some issues' project items,
// ProjectItems returns the rest with an *IncompleteError naming them: a
// refused issue isn't one that's off the project.
func (c *Client) ProjectItems(ctx context.Context, ids []string, statusField string) (map[string][]ProjectItem, error) {
	const q = `query($ids: [ID!]!, $statusField: String!) {
  nodes(ids: $ids) { ... on Issue { id projectItems(first: 20, includeArchived: false) { totalCount nodes { ...projectItemFields } } } }
}
fragment projectItemFields on ProjectV2Item {
  project { number owner { ... on Organization { login } ... on User { login } } }
  status: fieldValueByName(name: $statusField) {
    ... on ProjectV2ItemFieldSingleSelectValue { name }
  }
}`
	out := map[string][]ProjectItem{}
	var inc IncompleteError
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		var resp struct {
			Nodes []*struct {
				ID           string       `json:"id"`
				ProjectItems projectItems `json:"projectItems"`
			} `json:"nodes"`
		}
		err := c.query(ctx, q, map[string]any{"ids": batch, "statusField": statusFieldOrDefault(statusField)}, &resp)
		errs, isErrs := errors.AsType[Errors](err)
		if isErrs && errs.ignorable() {
			err = nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading project items: %w", err)
		}
		refused := errs.refused("nodes")
		for i, n := range resp.Nodes {
			switch {
			case n == nil && refused[i] && i < len(batch):
				inc.add(batch[i], errs)
			case n != nil && n.ID != "":
				n.ProjectItems.logTruncated(ctx, n.ID)
				out[n.ID] = n.ProjectItems.items()
			}
		}
	}
	if inc.Missing > 0 {
		return out, fmt.Errorf("reading project items: %w", &inc)
	}
	return out, nil
}
