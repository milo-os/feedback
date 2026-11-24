// Package github reads issues from GitHub's GraphQL API as the "Datum Cloud
// Feedback" GitHub App, with a token for each installation.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is GitHub's REST and GraphQL API.
const DefaultBaseURL = "https://api.github.com"

// tokenSlack is how long before expiry a cached installation token is
// replaced. GitHub issues tokens for an hour.
const tokenSlack = 5 * time.Minute

// App authenticates as a GitHub App and hands out installation tokens.
type App struct {
	id      int64
	key     *rsa.PrivateKey
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu      sync.Mutex
	tokens  map[int64]installationToken
	orgs    map[string]orgInstallation
	clients map[int64]*Client
}

// installationTTL is how long an org's installation ID is cached, so a
// reinstalled app is picked up without a restart.
const installationTTL = time.Hour

type orgInstallation struct {
	id        int64
	fetchedAt time.Time
}

type installationToken struct {
	token     string
	expiresAt time.Time
}

// AppOption configures an App.
type AppOption func(*App)

// WithBaseURL points the App at another API, such as a test server.
func WithBaseURL(u string) AppOption {
	return func(a *App) { a.baseURL = strings.TrimSuffix(u, "/") }
}

// WithHTTPClient sets the HTTP client.
func WithHTTPClient(c *http.Client) AppOption {
	return func(a *App) { a.http = c }
}

// NewApp returns an App for app ID id with the PEM-encoded private key.
func NewApp(id int64, privateKeyPEM []byte, opts ...AppOption) (*App, error) {
	if id <= 0 {
		return nil, errors.New("GitHub App ID must be set")
	}
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	a := &App{
		id:      id,
		key:     key,
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
		now:     time.Now,
		tokens:  map[int64]installationToken{},
		orgs:    map[string]orgInstallation{},
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

func parsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("GitHub App private key: no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("GitHub App private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key is not RSA")
	}
	return rk, nil
}

// jwt returns a JSON Web Token for the app, valid for nine minutes. GitHub
// allows ten; iat is backdated a minute for clock drift.
func (a *App) jwt() (string, error) {
	now := a.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.id, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token returns a token for the installation, cached until shortly before it
// expires.
func (a *App) Token(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[installationID]
	a.mu.Unlock()
	if ok && a.now().Before(t.expiresAt.Add(-tokenSlack)) {
		return t.token, nil
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.appRequest(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), &out); err != nil {
		return "", fmt.Errorf("creating a token for installation %d: %w", installationID, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("creating a token for installation %d: empty token", installationID)
	}
	a.mu.Lock()
	a.tokens[installationID] = installationToken{token: out.Token, expiresAt: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// InstallationID returns the app's installation ID on org.
func (a *App) InstallationID(ctx context.Context, org string) (int64, error) {
	key := strings.ToLower(org)
	a.mu.Lock()
	cached, ok := a.orgs[key]
	a.mu.Unlock()
	if ok && a.now().Sub(cached.fetchedAt) < installationTTL {
		return cached.id, nil
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := a.appRequest(ctx, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/installation", &out); err != nil {
		return 0, fmt.Errorf("finding the installation on %s: %w", org, err)
	}
	a.mu.Lock()
	a.orgs[key] = orgInstallation{id: out.ID, fetchedAt: a.now()}
	a.mu.Unlock()
	return out.ID, nil
}

// appRequest calls a REST endpoint authenticated as the app itself.
func (a *App) appRequest(ctx context.Context, method, path string, out any) error {
	jwt, err := a.jwt()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
