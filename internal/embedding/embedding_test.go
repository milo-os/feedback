package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClient(t *testing.T) {
	var auth string
	var req embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Out of order on purpose: the client must use index.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(Config{URL: srv.URL + "/v1/", Model: "m", Dimensions: 2, TokenFile: tok})
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Errorf("vectors not in input order: %v", vecs)
	}
	if auth != "Bearer t1" || req.Model != "m" || req.Dimensions != 2 || len(req.Input) != 2 {
		t.Errorf("request: auth=%q %+v", auth, req)
	}

	// Rotated tokens are picked up.
	_ = os.WriteFile(tok, []byte("t2"), 0o600)
	_, _ = c.Embed(context.Background(), []string{"a", "b"})
	if auth != "Bearer t2" {
		t.Errorf("token not re-read: %q", auth)
	}
	if _, err := c.Embed(context.Background(), []string{"only one"}); err == nil {
		t.Error("index out of range: want error")
	}
}

func TestClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such model", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := NewClient(Config{URL: srv.URL, Model: "m"}).Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "no such model") {
		t.Fatalf("got %v", err)
	}
}

func TestText(t *testing.T) {
	got := Text(" Title ", "", "<p>Hello <b>world</b></p><script>x</script>\n<ul><li>one</li></ul>")
	if want := "Title\n\nHello world x one"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
	long := strings.Repeat("é", MaxInputChars) // two bytes each
	if s := Truncate(long); len(s) > MaxInputChars || !utf8.ValidString(s) || len(s) < MaxInputChars-1 {
		t.Errorf("Truncate: %d bytes, valid=%v", len(s), utf8.ValidString(s))
	}
	if Vector([]float32{1, 0.5, -2}) != "[1,0.5,-2]" {
		t.Error(Vector([]float32{1, 0.5, -2}))
	}
}
