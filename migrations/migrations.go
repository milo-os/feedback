// Package migrations embeds the forward-only Postgres schema migrations for
// the feedback apiserver. They are applied by `feedback migrate`, run as a Job
// before the apiserver rolls out.
package migrations

import "embed"

// FS holds the SQL migration files.
//
//go:embed *.sql
var FS embed.FS
