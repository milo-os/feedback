package post

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/internal/storage/pg"
	v1 "go.miloapis.com/feedback/pkg/apis/feedback/v1alpha1"
)

// halfLife is the trending score's half-life.
const halfLife = 7 * 24 * time.Hour

// voter is one Datum vote, as far as the tally cares.
type voter struct {
	at    time.Time
	login string // linked GitHub username, lower-cased
}

func votersOf(ctx context.Context, q pg.Querier, post string) ([]voter, error) {
	rows, err := q.Query(ctx, `SELECT created_at, lower(coalesce(object->'status'->>'githubLogin', ''))
		FROM votes WHERE post_name = $1`, post)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (voter, error) {
		var v voter
		return v, r.Scan(&v.at, &v.login)
	})
}

// weight is a vote's contribution to the trending score.
func weight(at, now time.Time) float64 {
	age := now.Sub(at)
	if age < 0 {
		age = 0
	}
	return math.Pow(0.5, float64(age)/float64(halfLife))
}

// tally sets the server-owned vote fields from the post's Datum voters and
// its GitHub reactions (sync-owned, already in v).
//
//	total    = datum + github - linked
//	trending = Σ weight(vote) + Σ weight(👍 from someone who didn't also vote)
//
// Reactions without a time count towards the total but not trending.
func tally(v *v1.PostVotes, voters []voter, now time.Time) {
	logins := map[string]bool{}
	v.TrendingScore = 0
	for _, vt := range voters {
		if vt.login != "" {
			logins[vt.login] = true
		}
		v.TrendingScore += weight(vt.at, now)
	}
	seen := map[string]bool{}
	var linked int32
	for _, r := range v.GitHubReactions {
		l := strings.ToLower(r.Login)
		if seen[l] {
			continue
		}
		seen[l] = true
		if logins[l] {
			linked++
			continue
		}
		if !r.At.IsZero() {
			v.TrendingScore += weight(r.At.Time, now)
		}
	}
	// The reactions list can lag the count; never subtract more than the
	// count says.
	linked = min(linked, v.GitHub)
	v.Datum = int32(len(voters))
	v.Linked = linked
	v.Total = v.Datum + v.GitHub - linked
	// Round so hourly refreshes don't rewrite posts for float noise.
	v.TrendingScore = math.Round(v.TrendingScore*1e4) / 1e4
}

// Tally sets p's server-owned vote fields from the votes table.
func Tally(ctx context.Context, q pg.Querier, p *v1.Post, now time.Time) error {
	voters, err := votersOf(ctx, q, p.Name)
	if err != nil {
		return fmt.Errorf("load votes for %s: %w", p.Name, err)
	}
	tally(&p.Status.Votes, voters, now)
	return nil
}

// Retally recomputes the stored post's vote fields and writes them if they
// changed, bumping its resourceVersion. Missing posts are skipped: votes on
// a deleted post wait for it to come back. Runs in ctx's transaction, if
// any.
func Retally(ctx context.Context, db *pg.DB, name string, now time.Time) (bool, error) {
	changed := false
	err := db.InTx(ctx, func(ctx context.Context) error {
		q := db.Q(ctx)
		var raw []byte
		err := q.QueryRow(ctx, `SELECT coalesce(object->'status'->'votes', '{}') FROM posts
			WHERE scope_type = $1 AND name = $2 FOR UPDATE`, pg.ScopePlatform, name).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var v v1.PostVotes
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		before := v
		voters, err := votersOf(ctx, q, name)
		if err != nil {
			return err
		}
		tally(&v, voters, now)
		if v.Datum == before.Datum && v.Total == before.Total && v.Linked == before.Linked && v.TrendingScore == before.TrendingScore {
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE posts SET
				object = jsonb_set(object, '{status,votes}', $3::jsonb),
				votes_total = $4, trending_score = $5,
				resource_version = nextval('resource_version_seq')
			WHERE scope_type = $1 AND name = $2`, pg.ScopePlatform, name, b, v.Total, v.TrendingScore)
		changed = err == nil
		return err
	})
	return changed, err
}

// RefreshTrending re-tallies every post with votes or a trending score, so
// scores decay as votes age. It returns how many posts changed.
func RefreshTrending(ctx context.Context, db *pg.DB, now time.Time) (int, error) {
	rows, err := db.Pool.Query(ctx, `SELECT name FROM posts
		WHERE scope_type = $1 AND (trending_score > 0 OR votes_total > 0)`, pg.ScopePlatform)
	if err != nil {
		return 0, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range names {
		changed, err := Retally(ctx, db, name, now)
		if err != nil {
			return n, fmt.Errorf("retally %s: %w", name, err)
		}
		if changed {
			n++
		}
	}
	return n, nil
}

// RunTrending refreshes trending scores every interval until ctx ends.
func RunTrending(ctx context.Context, db *pg.DB, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := RefreshTrending(ctx, db, time.Now())
		switch {
		case err != nil && ctx.Err() == nil:
			klog.ErrorS(err, "Refreshing trending scores failed", "updated", n)
		case n > 0:
			klog.V(2).InfoS("Refreshed trending scores", "posts", n)
		}
	}
}

// moveVotes moves votes and subscriptions from a post merged into another.
// A user who already has one on the target keeps that one and loses the
// source's. Runs in ctx's transaction.
func moveVotes(ctx context.Context, q pg.Querier, from, to string) error {
	for _, table := range []string{"votes", "subscriptions"} {
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %[1]s s USING %[1]s t
			WHERE s.post_name = $1 AND t.post_name = $2
			  AND t.scope_type = s.scope_type AND t.scope_name = s.scope_name`, table), from, to); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET
				name = $2, post_name = $2,
				object = jsonb_set(jsonb_set(object, '{metadata,name}', to_jsonb($2::text)),
				                   '{spec,postRef,name}', to_jsonb($2::text)),
				resource_version = nextval('resource_version_seq')
			WHERE post_name = $1`, table), from, to); err != nil {
			return err
		}
	}
	return nil
}
