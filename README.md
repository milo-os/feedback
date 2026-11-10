# Feedback

Feedback, votes and roadmap, built in.

`feedback` serves the `feedback.miloapis.com` API: public roadmap posts synced
from GitHub, votes, submissions, security reports, releases and changelog
entries. It runs as an aggregated API server under Milo, like
[activity](https://github.com/milo-os/activity) and
[search](https://github.com/milo-os/search), so the website, cloud portal,
`datumctl`, Patch and the staff portal all use one API.

- Product: [Platform Feedback](https://github.com/datum-cloud/enhancements/blob/main/enhancements/platform/feedback/README.md)
- Design: [Platform Feedback: Architecture](https://github.com/datum-cloud/enhancements/pull/925)
- Tracking: [datum-cloud/enhancements#889](https://github.com/datum-cloud/enhancements/issues/889),
  service and API epic [#943](https://github.com/datum-cloud/enhancements/issues/943)

## Status

Early scaffold. The server runs and serves `FeedbackSource`. The other
resources in the design are tracked as sub-issues of the epic.

| Resource | Storage | State |
| --- | --- | --- |
| `FeedbackSource` | etcd | served |
| `Post`, `PostQuery`, `Release`, `ChangelogEntry` | Postgres | planned |
| `Vote`, `Subscription` | Postgres | planned |
| `Submission`, `SecurityReport`, `SecurityAcknowledgement` | Postgres | planned |

## Layout

```
cmd/feedback/            serve, migrate and version commands
internal/apiserver/      server wiring and API group installation
internal/postgres/       connection pool, readiness check, migrations runner
internal/registry/       REST storage, one package per resource
migrations/              forward-only SQL migrations (embedded)
pkg/apis/feedback/       feedback.miloapis.com/v1alpha1 types
pkg/generated/           generated clientset and OpenAPI (task generate)
config/base/             apiserver Deployment, Service, RBAC
config/components/       APIService registration
config/milo/             Milo IAM ProtectedResources, Roles, discovery
config/samples/          example resources
```

## Postgres

The apiserver is the only component that connects to Postgres. Other
components (`feedback-sync`, `feedback-controller`) write through the API so
every change is authorized in one place and reaches the audit log.

The DSN comes from `--postgres-dsn` or `FEEDBACK_POSTGRES_DSN`. In a cluster
it is read from the `dsn` key of the `feedback-postgres` Secret, which the
environment provides. The database needs the `vector` extension available.

Migrations are forward-only. `feedback migrate` applies them; the Deployment
runs it as an init container, and a Postgres session lock keeps replicas from
migrating at the same time. Add a migration as the next numbered file in
`migrations/`.

## Development

```bash
task build      # bin/feedback
task test
task generate   # after changing types in pkg/apis
task docker-build
```

Run Postgres with pgvector and apply migrations locally:

```bash
docker run -d --name feedback-pg -e POSTGRES_PASSWORD=feedback -p 127.0.0.1:5432:5432 pgvector/pgvector:pg17
FEEDBACK_POSTGRES_DSN=postgres://postgres:feedback@127.0.0.1:5432/postgres bin/feedback migrate
```

## License

[AGPL-3.0](LICENSE)
