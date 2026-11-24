package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means GitHub has no such repository or issue, or the
// installation can't see it.
var ErrNotFound = errors.New("not found on GitHub")

// RateLimit is the installation's GraphQL rate limit after the last request.
type RateLimit struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// Client runs GraphQL queries as one installation of the app.
type Client struct {
	app            *App
	installationID int64

	mu   sync.Mutex
	rate *RateLimit
}

// Client returns the GraphQL client for an installation. Clients are kept,
// so their rate limit survives between sweeps.
func (a *App) Client(installationID int64) *Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clients == nil {
		a.clients = map[int64]*Client{}
	}
	c, ok := a.clients[installationID]
	if !ok {
		c = &Client{app: a, installationID: installationID}
		a.clients[installationID] = c
	}
	return c
}

// RateLimit returns the rate limit GitHub reported on the last request, or
// nil before the first.
func (c *Client) RateLimit() *RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate == nil {
		return nil
	}
	r := *c.rate
	return &r
}

// gqlError is one entry of a GraphQL response's errors.
type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// Errors are the errors a GraphQL response carried alongside its data.
type Errors []gqlError

func (e Errors) Error() string {
	msgs := make([]string, 0, len(e))
	for _, x := range e {
		msgs = append(msgs, x.Message)
	}
	return "GitHub GraphQL: " + strings.Join(msgs, "; ")
}

// ignorable reports whether the data that came with these errors can be
// used: every error is NOT_FOUND (the node is null and the caller checks for
// it), or is about a project item. An installation can't read another org's
// project, so GitHub may refuse an item's project while returning the issue;
// feedback-sync reads such projects through their own org's installation.
func (e Errors) ignorable() bool {
	for _, x := range e {
		if x.Type != "NOT_FOUND" && !slices.Contains(x.Path, any("projectItems")) {
			return false
		}
	}
	return len(e) > 0
}

// refused returns the indexes in the list at prefix (such as "search",
// "nodes") that an error other than NOT_FOUND points into. An entry there
// that came back null was refused, not missing.
func (e Errors) refused(prefix ...any) map[int]bool {
	out := map[int]bool{}
	for _, x := range e {
		if x.Type == "NOT_FOUND" || len(x.Path) <= len(prefix) || !slices.Equal(x.Path[:len(prefix)], prefix) {
			continue
		}
		if i, ok := x.Path[len(prefix)].(float64); ok {
			out[int(i)] = true
		}
	}
	return out
}

// IncompleteError means GitHub refused some of the issues a query asked
// for. It comes back alongside the rest of the results, which leave those
// issues out.
type IncompleteError struct {
	// IDs are the node IDs of the refused issues, when the query named them.
	IDs []string
	// Missing is how many issues were refused.
	Missing int
	Err     Errors
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("GitHub refused %d issues: %v", e.Missing, e.Err)
}

func (e *IncompleteError) Unwrap() error { return e.Err }

func (e *IncompleteError) add(id string, errs Errors) {
	e.Missing++
	if id != "" {
		e.IDs = append(e.IDs, id)
	}
	for _, x := range errs {
		if !slices.ContainsFunc(e.Err, func(y gqlError) bool { return y.Message == x.Message }) {
			e.Err = append(e.Err, x)
		}
	}
}

// RateLimitError means GitHub refused a request for going over a rate limit,
// either the hourly one or a secondary one.
type RateLimitError struct {
	// RetryAt is when GitHub will take requests again.
	RetryAt time.Time
	Message string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub rate limit: %s (retry at %s)", e.Message, e.RetryAt.UTC().Format(time.RFC3339))
}

// secondaryRateLimitWait is how long to wait after a secondary rate limit
// that gives no Retry-After: GitHub asks for at least a minute.
const secondaryRateLimitWait = time.Minute

// rateLimited returns a RateLimitError if a response refused the request
// for a rate limit: a 429, or a 403 with Retry-After, no requests left, or a
// message about a rate limit.
func (a *App) rateLimited(h http.Header, status int, msg string) *RateLimitError {
	retryAfter, remaining := h.Get("Retry-After"), h.Get("X-RateLimit-Remaining")
	switch {
	case status == http.StatusTooManyRequests:
	case status == http.StatusForbidden && (retryAfter != "" || remaining == "0" || strings.Contains(strings.ToLower(msg), "rate limit")):
	default:
		return nil
	}
	return &RateLimitError{RetryAt: a.retryAt(h), Message: msg}
}

// retryAt is when a rate-limited request can be tried again: after
// Retry-After, else at the hourly limit's reset when it has run out, else
// after secondaryRateLimitWait.
func (a *App) retryAt(h http.Header) time.Time {
	now := a.now()
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return now.Add(time.Duration(s) * time.Second)
	}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && h.Get("X-RateLimit-Remaining") == "0" {
		return time.Unix(reset, 0).UTC()
	}
	return now.Add(secondaryRateLimitWait)
}

// notFound reports whether every error is NOT_FOUND. Only then does a
// missing node mean the issue is gone: an error on a non-null field, such
// as projectItems, also nulls the issue that holds it.
func (e Errors) notFound() bool {
	for _, x := range e {
		if x.Type != "NOT_FOUND" {
			return false
		}
	}
	return len(e) > 0
}

// query runs a GraphQL query and decodes data into out. It returns the
// response's errors as Errors, after decoding whatever data came with them,
// and a refusal for a rate limit as a *RateLimitError.
func (c *Client) query(ctx context.Context, q string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.recordRateLimit(resp.Header)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(raw))
		if rl := c.app.rateLimited(resp.Header, resp.StatusCode, msg); rl != nil {
			return rl
		}
		return fmt.Errorf("GitHub GraphQL: %s: %s", resp.Status, msg)
	}
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors Errors          `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("GitHub GraphQL: decoding response: %w", err)
	}
	if len(r.Data) > 0 && string(r.Data) != "null" {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return fmt.Errorf("GitHub GraphQL: decoding data: %w", err)
		}
	}
	for _, e := range r.Errors {
		if e.Type == "RATE_LIMITED" {
			return &RateLimitError{RetryAt: c.app.retryAt(resp.Header), Message: e.Message}
		}
	}
	if len(r.Errors) > 0 {
		return r.Errors
	}
	return nil
}

// post sends a GraphQL request, getting a new token once if GitHub refuses
// the cached one.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.app.Token(ctx, c.installationID)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.app.baseURL+"/graphql", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.app.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.app.forgetToken(c.installationID, token)
			continue
		}
		return resp, nil
	}
}

func (c *Client) recordRateLimit(h http.Header) {
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	c.mu.Lock()
	c.rate = &RateLimit{Limit: limit, Remaining: rem, ResetAt: time.Unix(reset, 0).UTC()}
	c.mu.Unlock()
}

// forgetToken drops a token GitHub refused, unless another request has
// already replaced it.
func (a *App) forgetToken(installationID int64, token string) {
	a.mu.Lock()
	if a.tokens[installationID].token == token {
		delete(a.tokens, installationID)
	}
	a.mu.Unlock()
}
