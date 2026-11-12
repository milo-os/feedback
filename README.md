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
serves every resource in the design. Storage for the rest is tracked as
sub-issues of the epic.

| Resource | Storage | State |
| --- | --- | --- |
| `FeedbackSource` | etcd | served |
| `Post`, `Release`, `ChangelogEntry` | Postgres | served |
| `PostQuery` | Postgres (not stored) | served |
| `Vote`, `Subscription` | Postgres | served |
| `Submission`, `AttachmentUpload` | Postgres, bucket | served |
| `SecurityReport`, `SecurityAcknowledgement` | Postgres, bucket | served |

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

## Access

`config/milo` registers the service with Milo:

- `iam/`: a `ProtectedResource` per kind (users act on their own `User`;
  staff act platform-wide) and roles:

  | Role | For |
  | --- | --- |
  | `feedback.miloapis.com-user` | every signed-in user, in their own user context |
  | `feedback.miloapis.com-public-reader` | the website: visible posts, releases, changelog, acknowledgements |
  | `feedback.miloapis.com-viewer` | staff read access, including hidden posts |
  | `feedback.miloapis.com-reviewer` | the feedback queue: submissions, reviews, hiding posts |
  | `feedback.miloapis.com-editor`, `-admin` | managing `FeedbackSource`s |
  | `feedback.miloapis.com-security-team` | security reports; no other role can see them |

- `rbac/`: Kubernetes RBAC in Milo for the service's own components, which
  authenticate with client certificates:
  `system:feedback-sync@feedback.miloapis.com`,
  `system:feedback-controller@feedback.miloapis.com` and
  `system:feedback-apiserver@feedback.miloapis.com`. Each is limited to its
  own reads and writes; none needs `system:masters`.
- `discovery/`: everything users touch appears in the platform and user
  contexts; `FeedbackSource` only in the platform context.
- `bindings/` (optional): the `feedback-security-team` group with the
  security-team role, and the reviewer role for `staff-users`.

Users get the user role by adding it to `inheritedRoles` of Milo's
`iam-user-self-manage` role. That role is shared with other services, so
the deployment patches it and keeps their entries.

The apiserver enforces the contexts too: votes and subscriptions change
only in their owner's context, submissions and security reports are created
only by their author (or logged by the Security Team), and
`FeedbackSource` is platform-only.

## Search

`config/milo/search` registers a `ResourceIndexPolicy` each for `Post`,
`Release` and `ChangelogEntry` with Milo's search service, which indexes them
from the audit events this apiserver writes. That is what PostQuery's `text`
uses (with `--milo-kubeconfig`), and it makes feedback findable through
platform search.

- Searchable: titles, summaries and body text, categories, labels and post
  state. `votes`, `trendingScore`, `delivery` and the derived post lists
  aren't searchable: they change as side effects and would go stale.
- Hidden posts aren't indexed; hiding a post removes it from the index.
- Submissions, security reports, votes and subscriptions are never indexed.

`hack/search-lag.sh <post>` measures how long a change takes to reach the
index, by hiding and unhiding a post. Run it against staging.

## Audit

Milo proxies requests to this API without their bodies, so the apiserver is
the audit source for its own writes (`config/components/audit-logging`,
included in the core-control-plane overlay):

- Writes to posts, releases, changelog entries, votes, subscriptions and
  feedback sources are logged at `RequestResponse`. The search indexer reads
  posts, releases and changelog entries from `responseObject`.
- Submissions, security reports, uploads and `PostQuery` are logged at
  `Metadata`, so private context, vulnerability details, files and draft
  feedback never reach the audit log, activity or search.
- Reads aren't logged here; Milo's events cover them.
- Side effects are annotations on the request that caused them:
  `feedback.miloapis.com/subscribed`, `votes-moved-to`, `vote-granted` and
  `security-report`.

Events go to activity's Vector webhook in `activity-system`, like Milo's.
The deployment supplies the webhook's CA as the `feedback-audit-webhook-ca`
ConfigMap, and drops Milo's own events for `feedback.miloapis.com` writes so
each write is recorded once.

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
(`--sync-users`, `--controller-users`). The defaults are the common names of
the client certificates they present to Milo:
`system:feedback-sync@feedback.miloapis.com` and
`system:feedback-controller@feedback.miloapis.com`. On top of Milo's IAM:

- Only `feedback-sync` creates and deletes posts, and writes releases and
  changelog entries.
- Post status is split: `feedback-sync` owns the GitHub-derived fields and
  delivery work items; `feedback-controller` owns the delivery stage,
  history, `highestNotified` and part-shipped records; the server computes
  vote counts. Fields a caller doesn't own are ignored, like spec on a
  status update. Anyone else gets Forbidden on post status.
- Staff can change `spec.hidden` and metadata. Hidden posts are visible only
  to callers allowed `get` on `posts/hidden` (staff, sync, the controller),
  in any context; everyone else, the website included, gets NotFound. They
  are never listed on releases or changelog entries or returned by
  `PostQuery`.
- `Release.status.posts` and `ChangelogEntry.status.posts` are derived from
  the posts table on every read.
- Votes on a deleted post are kept for `--vote-retention` (30 days) in case
  the post comes back.

## Votes

`Vote` and `Subscription` belong to a user. Users create, read and delete
their own in the user context; staff list everyone's in the platform
context, where the `feedback.miloapis.com/owner` annotation names the owner.
Both are named after the post, so there is one of each per user per post.

- Voting needs a verified email (the `iam.miloapis.com/emailVerified` claim
  Milo puts on human identities) and a visible post that hasn't been merged.
- A vote also subscribes the voter (`reason: voted`), and recomputes the
  post's `votes` in the same transaction. Unvoting keeps the subscription.
  Users following a post themselves get `reason: manual`.
- The vote records the voter's organizations and linked GitHub username,
  looked up from Milo (`--milo-kubeconfig`). They are for staff analytics and
  dedupe, never public. If Milo can't answer, the vote is recorded without
  them.
- `votes.total` is Datum votes plus GitHub 👍, minus `votes.linked`: voters
  whose linked GitHub username is among the 👍 reactions `feedback-sync`
  reports in `votes.githubReactions`.
- `votes.trendingScore` is `Σ 0.5^(age_days / 7)` over votes and 👍
  reactions with a known time, counting linked people once. It is
  recomputed on every vote and post write, and hourly as votes age.
- When `feedback-sync` sets `status.mergedInto` on a post, its votes and
  subscriptions move to the target. A user with both keeps one.

## PostQuery

`PostQuery` is create-to-query: send one, read `status.results`, `facets`
and `continue`. Nothing is stored. Hidden posts never appear.

- **Browse:** filter by state, categories, orgs and delivery stage; sort by
  `top`, `newest`, `trending` or `recentlyChanged`. `votedByMe` and
  `followedByMe` work in the user context only.
- **`text`** goes to Milo's search service as a `ResourceSearchQuery` over the
  posts index (`--milo-kubeconfig`), then filters apply in Postgres and
  `relevance` keeps search's order. If search is down, `text` returns 503.
  Without `--milo-kubeconfig` it uses Postgres keyword matching instead,
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

## Submissions

Private feedback waiting for a staff decision.

- **Submit:** users create submissions in the user context, with a verified
  email, at most `--submission-limit` (5) per `--submission-window` (10m),
  counted in Postgres across replicas. Names are always generated (`sub-…`)
  because they are unique across users. Users can't change or withdraw a
  submission.
- **Read:** submitters read their own, without `triage`, the reviewer or the
  staff-edited public text. Staff list and read all of them in the platform
  context, with the owner in the `feedback.miloapis.com/owner` annotation.
- **Status:** `feedback-controller` writes `status.triage` and
  `feedback-sync` writes `status.postRef` (once). Nobody else writes status.
- **Review:** staff `create` on `submissions/{name}/review` with a
  `SubmissionReview`: `Approved` (public title and body), `Merged`
  (`mergedInto`, a visible post) or `Rejected` (a reason the submitter
  sees). Only from `PendingReview`, only in the platform context, and never
  by feedback-sync or feedback-controller.
- **Votes:** a merge gives the submitter a `Vote` and a `Subscription`
  (`reason: merged`) in the same transaction. An approval does the same
  (`reason: submitted`) when feedback-sync links the new post.

**Attachments.** Clients create an `AttachmentUpload` in the user context
(PNG, JPEG, GIF, WebP, PDF, plain text or JSON; `--attachment-max-size`,
2 MiB; the declared type must match the content), then list its
`uploadRef` in the submission. The server copies the file under the
submission and takes its name and type from the upload, not the client.
`GET submissions/{name}/attachment?ref=…` returns the file, as a download,
to the submitter and staff. Files live in `--attachments-bucket`
(`gs://…` in clusters, `file://` or `mem://` locally); give the bucket a
lifecycle rule deleting `uploads/` after a day. Without a bucket,
attachments are off and submissions without them still work.

## Security reports

Private vulnerability reports, with their own role: only the Security Team
can list them or write their status. Feedback reviewers can't see them;
Milo IAM enforces that (see #10).

- **Report:** reporters create reports in the user context (verified email,
  same rate limit as submissions) and read their own, without
  `status.internal`. Names are generated (`sec-…`). Attachments are
  `AttachmentUpload`s with `purpose: SecurityReport`, stored under their own
  prefix and served by `securityreports/{name}/attachment?ref=` to the
  reporter and the Security Team.
- **Log:** the Security Team logs reports from email or GitHub in the
  platform context with `spec.reporter` set. Those belong to the platform.
- **Work:** the Security Team writes `status` through the status
  subresource. A phase change gets a timeline entry if the team didn't add
  one; timeline entries can be added but never changed. `NotApplicable` and
  `Duplicate` need a reason, `Duplicate` needs `internal.duplicateOf`, and
  `credits` rewards need an amount.
- **Move:** staff send a misfiled submission to the security track with
  `create` on `submissions/{name}/to-security-report`. In one transaction a
  report owned by the submitter is created from it, its attachments are
  copied, and the submission is deleted.
- **Acknowledge:** `SecurityAcknowledgement` lists reports in `Fixed` or
  `Disclosed` whose reporter chose public credit, with only the credited
  name, link, month fixed and severity.

Reports are never embedded, indexed, synced or sent to the AI gateway.

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
