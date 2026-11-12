// Package embedding turns post text into vectors through the internal AI
// gateway and keeps the post_embeddings table current.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Embedder turns texts into vectors, one per input, in order.
type Embedder interface {
	// Model names the vectors' model; vectors from different models are
	// never compared.
	Model() string
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Config points at an OpenAI-compatible /embeddings endpoint, such as the
// internal AI gateway.
type Config struct {
	// URL is the API base, including /v1 (the client appends /embeddings).
	URL string
	// Model is the embedding model the gateway routes.
	Model string
	// Dimensions asks the model for shorter vectors, when it supports that.
	// Zero leaves the model's default.
	Dimensions int
	// TokenFile holds the bearer token, re-read on every request so rotated
	// projected service account tokens are picked up.
	TokenFile string
	// Timeout bounds each request.
	Timeout time.Duration
}

// Enabled reports whether embeddings are configured.
func (c Config) Enabled() bool { return c.URL != "" && c.Model != "" }

// Client calls the gateway.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient returns a client for cfg.
func NewClient(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Model implements Embedder.
func (c *Client) Model() string { return c.cfg.Model }

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed implements Embedder.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.cfg.Model, Input: inputs, Dimensions: c.cfg.Dimensions})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.cfg.URL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.TokenFile != "" {
		tok, err := os.ReadFile(c.cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read AI gateway token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("AI gateway returned %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode AI gateway response: %w", err)
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) || len(d.Embedding) == 0 {
			return nil, errors.New("AI gateway returned a malformed embedding")
		}
		vecs[d.Index] = d.Embedding
	}
	for _, v := range vecs {
		if v == nil {
			return nil, errors.New("AI gateway returned fewer embeddings than inputs")
		}
	}
	return vecs, nil
}
