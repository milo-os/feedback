// Package postgres manages the feedback apiserver's Postgres connection and
// schema migrations. The apiserver is the only component that connects to
// Postgres; every other component writes through the Feedback API.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"k8s.io/klog/v2"

	"go.miloapis.com/feedback/migrations"
)

// Config holds connection settings.
type Config struct {
	// DSN is a libpq connection string or postgres:// URL.
	DSN string
	// MaxConns caps the pool size. Zero uses the pgx default.
	MaxConns int32
	// ConnectTimeout bounds the initial connection attempt.
	ConnectTimeout time.Duration
}

// Validate checks that the config can be used to connect.
func (c Config) Validate() error {
	if c.DSN == "" {
		return errors.New("a Postgres DSN is required")
	}
	_, err := pgxpool.ParseConfig(c.DSN)
	return err
}

// NewPool opens a connection pool and verifies it with a ping.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// HealthCheck is a readyz check that pings the pool.
type HealthCheck struct {
	Pool *pgxpool.Pool
}

// Name implements healthz.HealthChecker.
func (HealthCheck) Name() string { return "postgres" }

// Check implements healthz.HealthChecker.
func (h HealthCheck) Check(r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	return h.Pool.Ping(ctx)
}

// Migrate applies every pending migration. Migrations are forward-only.
func Migrate(ctx context.Context, cfg Config) error {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return fmt.Errorf("parse postgres DSN: %w", err)
	}
	db := stdlib.OpenDB(*pc.ConnConfig)
	defer func() { _ = db.Close() }()
	return migrate(ctx, db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	// A session lock stops two replicas' init containers migrating at once.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	for _, r := range results {
		klog.InfoS("Applied migration", "version", r.Source.Version, "path", r.Source.Path, "duration", r.Duration)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	v, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	klog.InfoS("Schema is up to date", "version", v)
	return nil
}
