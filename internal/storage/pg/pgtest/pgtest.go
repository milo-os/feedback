// Package pgtest starts a throwaway pgvector Postgres for integration tests
// and applies the real migrations.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"

	"go.miloapis.com/feedback/internal/postgres"
	"go.miloapis.com/feedback/internal/storage/pg"
)

// DB is set by Main when Postgres started; tests call Require to skip
// without it.
var DB *pg.DB

// Main starts Postgres, runs the package's tests and stops it. Call it from
// TestMain: os.Exit(pgtest.Main(m)). Without Docker the tests still run and
// skip themselves through Require.
func Main(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("feedback"),
		tcpostgres.WithUsername("feedback"),
		tcpostgres.WithPassword("feedback"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipping Postgres tests: %v\n", err)
		return m.Run()
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg := postgres.Config{DSN: dsn}
	if err := postgres.Migrate(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	DB = &pg.DB{Pool: pool}
	return m.Run()
}

// Require skips the test without Postgres and empties tables.
func Require(t *testing.T, tables ...string) *pg.DB {
	t.Helper()
	if DB == nil {
		t.Skip("Postgres not available (needs Docker)")
	}
	if len(tables) > 0 {
		if _, err := DB.Pool.Exec(context.Background(), "TRUNCATE "+strings.Join(tables, ", ")); err != nil {
			t.Fatal(err)
		}
	}
	return DB
}

// requestContext is what the apiserver passes a cluster-scoped request.
func requestContext() context.Context {
	return request.WithNamespace(context.Background(), "")
}

// AsUser is a request from a user in the user context.
func AsUser(uid string) context.Context {
	return request.WithUser(requestContext(), &user.DefaultInfo{
		Name: uid + "@example.com", UID: uid,
		Extra: map[string][]string{pg.ParentKindExtraKey: {"User"}, pg.ParentNameExtraKey: {uid}},
	})
}

// AsPlatform is a request from name in the platform context.
func AsPlatform(name string) context.Context {
	return request.WithUser(requestContext(), &user.DefaultInfo{Name: name, UID: name})
}

// AsContext is a request from a user in another parent context, e.g.
// Organization.
func AsContext(kind, name string) context.Context {
	return request.WithUser(requestContext(), &user.DefaultInfo{Name: "someone", Extra: map[string][]string{
		pg.ParentKindExtraKey: {kind}, pg.ParentNameExtraKey: {name}}})
}
