// Package milo reads what the feedback service needs to know about a user
// from the Milo API: their organizations and linked GitHub username.
package milo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Users looks users up. Lookups are best effort: a vote is still recorded
// when Milo can't answer.
type Users interface {
	// Organizations returns the names of the organizations user belongs to,
	// sorted.
	Organizations(ctx context.Context, user string) ([]string, error)
	// GitHubLogin returns the GitHub username linked to the user, or "".
	GitHubLogin(ctx context.Context, user string) (string, error)
}

// Client calls the Milo API with the apiserver's own credentials.
type Client struct {
	host   string
	client *http.Client
}

// Config loads a rest config from kubeconfig.
func Config(kubeconfig string) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load Milo kubeconfig: %w", err)
	}
	return cfg, nil
}

// NewClient returns a client for the Milo API server in cfg.
func NewClient(cfg *rest.Config) (*Client, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{host: strings.TrimSuffix(cfg.Host, "/"), client: hc}, nil
}

type list[T any] struct {
	Items []T `json:"items"`
}

type membership struct {
	Spec struct {
		OrganizationRef struct {
			Name string `json:"name"`
		} `json:"organizationRef"`
	} `json:"spec"`
}

type userIdentity struct {
	Status struct {
		ProviderName string `json:"providerName"`
		Username     string `json:"username"`
	} `json:"status"`
}

// Organizations implements Users.
func (c *Client) Organizations(ctx context.Context, user string) ([]string, error) {
	var l list[membership]
	if err := c.get(ctx, "/apis/resourcemanager.miloapis.com/v1alpha1/organizationmemberships", "spec.userRef.name="+user, &l); err != nil {
		return nil, err
	}
	var orgs []string
	for _, m := range l.Items {
		if n := m.Spec.OrganizationRef.Name; n != "" && !slices.Contains(orgs, n) {
			orgs = append(orgs, n)
		}
	}
	slices.Sort(orgs)
	return orgs, nil
}

// GitHubLogin implements Users.
func (c *Client) GitHubLogin(ctx context.Context, user string) (string, error) {
	var l list[userIdentity]
	if err := c.get(ctx, "/apis/identity.miloapis.com/v1alpha1/useridentities", "status.userUID="+user, &l); err != nil {
		return "", err
	}
	for _, id := range l.Items {
		if strings.Contains(strings.ToLower(id.Status.ProviderName), "github") && id.Status.Username != "" {
			return id.Status.Username, nil
		}
	}
	return "", nil
}

func (c *Client) get(ctx context.Context, path, fieldSelector string, out any) error {
	u := c.host + path + "?" + url.Values{"fieldSelector": {fieldSelector}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
