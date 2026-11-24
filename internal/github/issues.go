package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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
  labels(first: 100) { nodes { name } }
  projectItems(first: 20, includeArchived: false) { nodes { ...projectItemFields } }
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
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	ProjectItems projectItems `json:"projectItems"`
}

type projectItems struct {
	Nodes []struct {
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
		it := ProjectItem{Owner: n.Project.Owner.Login, Number: n.Project.Number}
		if n.Status != nil {
			it.Status = n.Status.Name
		}
		out = append(out, it)
	}
	return out
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
	if errs, ok := errors.AsType[Errors](err); ok && errs.onlyNotFound() {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if out.Repository == nil || out.Repository.Issue == nil {
		return nil, ErrNotFound
	}
	is := out.Repository.Issue.issue()
	return &is, nil
}

// searchLimit is the most results GitHub's search returns for one query.
const searchLimit = 1000

// searchPageSize keeps each page's cost and size down: bodies can be large.
const searchPageSize = 50

// earliest is before any GitHub issue was created.
var earliest = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)

// SearchIssues returns every issue matching an issue search query, such as
// `org:datum-cloud is:issue is:public label:"Build"`. GitHub returns at most
// 1000 results per query, so a query with more is split by creation date.
func (c *Client) SearchIssues(ctx context.Context, query, statusField string, now time.Time) ([]Issue, error) {
	issues, err := c.searchAll(ctx, query, statusField)
	if !errors.Is(err, errTooMany) {
		return issues, err
	}
	return c.searchRange(ctx, query, statusField, earliest, now.Add(24*time.Hour))
}

var errTooMany = errors.New("more search results than GitHub returns")

func (c *Client) searchRange(ctx context.Context, query, statusField string, from, to time.Time) ([]Issue, error) {
	q := fmt.Sprintf("%s created:%s..%s", query, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	issues, err := c.searchAll(ctx, q, statusField)
	if !errors.Is(err, errTooMany) {
		return issues, err
	}
	if to.Sub(from) <= time.Minute {
		return nil, fmt.Errorf("searching %q: more than %d issues created in one minute", query, searchLimit)
	}
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
	a, err := c.searchRange(ctx, query, statusField, from, mid)
	if err != nil {
		return nil, err
	}
	// Ranges are inclusive at both ends, so an issue created at mid is in
	// both halves; callers dedupe by ID.
	b, err := c.searchRange(ctx, query, statusField, mid, to)
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

// searchAll pages through one query. It returns errTooMany, without paging,
// when the query has more results than GitHub returns.
func (c *Client) searchAll(ctx context.Context, query, statusField string) ([]Issue, error) {
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
		if err := c.query(ctx, q, map[string]any{
			"q": query, "first": searchPageSize, "after": after, "statusField": statusFieldOrDefault(statusField),
		}, &out); err != nil {
			return nil, fmt.Errorf("searching %q: %w", query, err)
		}
		if out.Search.IssueCount > searchLimit {
			return nil, errTooMany
		}
		for _, n := range out.Search.Nodes {
			if n != nil && n.ID != "" {
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
// aren't found are left out.
func (c *Client) ProjectItems(ctx context.Context, ids []string, statusField string) (map[string][]ProjectItem, error) {
	const q = `query($ids: [ID!]!, $statusField: String!) {
  nodes(ids: $ids) { ... on Issue { id projectItems(first: 20, includeArchived: false) { nodes { ...projectItemFields } } } }
}
fragment projectItemFields on ProjectV2Item {
  project { number owner { ... on Organization { login } ... on User { login } } }
  status: fieldValueByName(name: $statusField) {
    ... on ProjectV2ItemFieldSingleSelectValue { name }
  }
}`
	out := map[string][]ProjectItem{}
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		var resp struct {
			Nodes []*struct {
				ID           string       `json:"id"`
				ProjectItems projectItems `json:"projectItems"`
			} `json:"nodes"`
		}
		err := c.query(ctx, q, map[string]any{"ids": batch, "statusField": statusFieldOrDefault(statusField)}, &resp)
		if errs, ok := errors.AsType[Errors](err); ok && errs.onlyNotFound() {
			err = nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading project items: %w", err)
		}
		for _, n := range resp.Nodes {
			if n != nil && n.ID != "" {
				out[n.ID] = n.ProjectItems.items()
			}
		}
	}
	return out, nil
}
