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

Early. Every resource in the design has a v1alpha1 type in
`pkg/apis/feedback/v1alpha1` and a sample in `config/samples`, and the server
serves `FeedbackSource`, `Post`, `PostQuery`, `Release` and `ChangelogEntry`. Storage for the rest is tracked as
sub-issues of the epic.

| Resource | Storage | State |
| --- | --- | --- |
| `FeedbackSource` | etcd | served |
| `Post`, `Release`, `ChangelogEntry` | Postgres | served |
| `PostQuery` | Postgres (not stored) | served |
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

## Who writes what

`feedback-sync` and `feedback-controller` are recognised by username
(`--sync-users`, `--controller-users`; defaults are the `feedback-system`
service accounts). On top of Milo's IAM:

- Only `feedback-sync` creates and deletes posts, and writes releases and
  changelog entries.
- Post status is split: `feedback-sync` owns the GitHub-derived fields and
  delivery work items; `feedback-controller` owns the delivery stage,
  history, `highestNotified` and part-shipped records; the server computes
  vote counts. Fields a caller doesn't own are ignored, like spec on a
  status update. Anyone else gets Forbidden on post status.
- Staff can change `spec.hidden` and metadata. Hidden posts are NotFound
  outside the platform context and never listed on releases or changelog
  entries.
- `Release.status.posts` and `ChangelogEntry.status.posts` are derived from
  the posts table on every read.
- Votes on a deleted post are kept for `--vote-retention` (30 days) in case
  the post comes back.

## PostQuery

`PostQuery` is create-to-query: send one, read `status.results`, `facets`
and `continue`. Nothing is stored. Hidden posts never appear.

- **Browse:** filter by state, categories, orgs and delivery stage; sort by
  `top`, `newest`, `trending` or `recentlyChanged`. `votedByMe` and
  `followedByMe` work in the user context only.
- **`text`** goes to Milo's search service as a `ResourceSearchQuery` over the
  posts index (`--search-kubeconfig`), then filters apply in Postgres and
  `relevance` keeps search's order. If search is down, `text` returns 503.
  Without `--search-kubeconfig` it uses Postgres keyword matching instead,
  which is what local development gets.
- **`similarTo`** embeds the draft text through the AI gateway and ranks the
  100 nearest posts by cosine similarity. If the gateway is unreachable, or
  no posts have embeddings yet, it falls back to keyword matching and says so
  in a `Warning` header. It is rate limited per user (`--similar-to-rate`,
  `--similar-to-burst`), per replica, with 429 and `Retry-After`.
- **Embeddings** are made by the apiserver itself every `--embed-interval`
  for new and changed posts (title, summary and body as text), stored in
  `post_embeddings` with their model. Changing `--embedding-model`
  re-embeds everything. One replica does this at a time.

The gateway is any OpenAI-compatible `/embeddings` endpoint:
`--ai-gateway-url` (including `/v1`), `--embedding-model`, and
`--ai-gateway-token-file` for a projected service account token. Leave the
URL empty to run without embeddings.

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
