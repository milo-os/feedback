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

// query runs a GraphQL query and decodes data into out. It returns the
// response's errors as Errors, after decoding whatever data came with them.
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
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub GraphQL: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
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
