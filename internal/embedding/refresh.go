package embedding

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/storage/pg"
)

// MaxInputChars caps the text sent for one post or query. Titles and
// summaries come first, so long bodies are what gets cut.
const MaxInputChars = 8000

// refreshLock is the advisory lock key that keeps replicas from embedding the
// same posts at once.
const refreshLock = 0x66656564 // "feed"

// contentHash is computed in SQL so any change to the embedded text, however
// it was written, marks the embedding stale.
const contentHash = `md5(coalesce(p.object->'status'->>'title', '') || chr(10) ||
	coalesce(p.object->'status'->>'summary', '') || chr(10) ||
	coalesce(p.object->'status'->>'bodyHTML', ''))`

// Text is what gets embedded for a post: title, summary and body as plain
// text, capped at MaxInputChars. Queries embed the user's text as is.
func Text(title, summary, bodyHTML string) string {
	var b strings.Builder
	for _, s := range []string{title, summary, StripHTML(bodyHTML)} {
		if s = strings.TrimSpace(s); s != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(s)
		}
	}
	return Truncate(b.String())
}

// Truncate cuts s to MaxInputChars without splitting a UTF-8 sequence.
func Truncate(s string) string {
	if len(s) <= MaxInputChars {
		return s
	}
	s = s[:MaxInputChars]
	// Drop a rune cut in half at the end (at most three bytes).
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// StripHTML returns the text content of an HTML fragment, with whitespace
// collapsed.
func StripHTML(s string) string {
	if s == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.TextToken:
			b.Write(z.Text())
			b.WriteByte(' ')
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			b.WriteByte(' ')
		}
	}
}

// Vector formats v as a pgvector literal, for a $n::vector parameter.
func Vector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Refresh embeds visible posts whose embedding is missing, from another
// model, or older than their text, batch at a time. It returns how many it
// embedded. If another replica holds the refresh lock it does nothing.
func Refresh(ctx context.Context, db *pg.DB, e Embedder, batch int) (int, error) {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", refreshLock).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", refreshLock)
	}()

	total := 0
	for {
		n, err := refreshBatch(ctx, db, e, batch)
		total += n
		if err != nil || n < batch {
			return total, err
		}
	}
}

type stale struct {
	name, hash, text string
}

func refreshBatch(ctx context.Context, db *pg.DB, e Embedder, batch int) (int, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT p.name, `+contentHash+`,
			coalesce(p.object->'status'->>'title', ''),
			coalesce(p.object->'status'->>'summary', ''),
			coalesce(p.object->'status'->>'bodyHTML', '')
		FROM posts p
		LEFT JOIN post_embeddings e ON e.post_name = p.name
		WHERE NOT p.hidden
		  AND (e.post_name IS NULL OR e.model <> $1 OR e.content_hash <> `+contentHash+`)
		ORDER BY p.name
		LIMIT $2`, e.Model(), batch)
	if err != nil {
		return 0, err
	}
	var todo []stale
	for rows.Next() {
		var s stale
		var title, summary, body string
		if err := rows.Scan(&s.name, &s.hash, &title, &summary, &body); err != nil {
			rows.Close()
			return 0, err
		}
		s.text = Text(title, summary, body)
		if s.text == "" {
			s.text = s.name
		}
		todo = append(todo, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(todo) == 0 {
		return 0, err
	}

	inputs := make([]string, len(todo))
	for i, s := range todo {
		inputs[i] = s.text
	}
	vecs, err := e.Embed(ctx, inputs)
	if err != nil {
		return 0, fmt.Errorf("embed %d posts: %w", len(todo), err)
	}
	for i, s := range todo {
		_, err := db.Pool.Exec(ctx, `
			INSERT INTO post_embeddings (post_name, model, content_hash, embedding, updated_at)
			VALUES ($1, $2, $3, $4::vector, now())
			ON CONFLICT (post_name) DO UPDATE
			SET model = excluded.model, content_hash = excluded.content_hash,
			    embedding = excluded.embedding, updated_at = excluded.updated_at`,
			s.name, e.Model(), s.hash, Vector(vecs[i]))
		if err != nil {
			return i, err
		}
	}
	return len(todo), nil
}

// Run refreshes embeddings every interval until ctx ends.
func Run(ctx context.Context, db *pg.DB, e Embedder, interval time.Duration) {
	klog.InfoS("Embedding posts", "model", e.Model(), "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := Refresh(ctx, db, e, 32)
		switch {
		case err != nil && ctx.Err() == nil:
			klog.ErrorS(err, "Embedding posts failed", "embedded", n)
		case n > 0:
			klog.InfoS("Embedded posts", "count", n, "model", e.Model())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
