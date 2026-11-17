# Feedback

Feedback, votes and roadmap, built in.

`feedback` defines the `feedback.miloapis.com` resources: public roadmap
posts synced from GitHub, votes, submissions, security reports, releases and
changelog entries. They are CRDs installed in Milo, like billing's, so the
website, cloud portal, `datumctl`, Patch and the staff portal all use Milo's
API. The `feedback` operator reconciles them and serves their admission
webhooks.

- Product: [Platform Feedback](https://github.com/datum-cloud/enhancements/blob/main/enhancements/platform/feedback/README.md)
- Design: [Platform Feedback: Architecture](https://github.com/datum-cloud/enhancements/pull/925)
- Tracking: [datum-cloud/enhancements#889](https://github.com/datum-cloud/enhancements/issues/889),
  service and API epic [#943](https://github.com/datum-cloud/enhancements/issues/943)

## Status

The CRDs are defined. The operator sets owners and reviewers in admission,
reconciles votes, follows, submission reviews and security reports, and
stores attachments. Activity and search are configured. Triage,
notifications and digests are tracked as sub-issues of the epic.

| Resource | Who writes it | Who reads it |
| --- | --- | --- |
| `FeedbackSource` | staff | `feedback-sync` |
| `Post`, `Release`, `ChangelogEntry` | `feedback-sync`; the operator writes post votes and delivery | everyone |
| `Vote`, `Subscription`, `FeedbackProfile` | users, for themselves; the operator | the owner, staff |
| `Submission` | users, for themselves; the operator writes status | the owner, staff |
| `SubmissionReview` | staff decide; the operator writes triage | staff |
| `SecurityReport` | users, for themselves; the Security Team writes status | the owner, the Security Team |
| `SecurityReportTriage` | the Security Team | the Security Team |
| `SecurityAcknowledgement`, `FeedbackDigest` | the operator | staff, the website |

## Who sees what

Milo stores user-owned resources in one platform-wide collection. Each
user-owned CRD names its owner field with the `iam.miloapis.com/owner-field`
annotation, and Milo limits user-context requests to the caller's objects
([milo-os/milo#1017](https://github.com/milo-os/milo/pull/1017)). `Post`
names `spec.hidden` with `iam.miloapis.com/hidden-field`, so hidden posts are
invisible to anyone without `get` on `posts/hidden`. Fields only staff may
see live on separate staff-only kinds (`SubmissionReview`,
`SecurityReportTriage`), since a CRD can't hide a field from some readers.

Vote and Subscription names are `<post>.<user>`, which gives one per user per
post across the shared collection.

The `owner` admission webhook sets `spec.userRef` on every user-owned kind
from the request: in a user's own context it is always that user. Only the
operator creates them for someone else, and the Security Team logs reports
for external reporters with no owner. The `reviewer` webhook records who
decided on a `SubmissionReview`.

## Votes

- **Admission** (`vote` webhook). A vote or follow needs a post that exists, isn't hidden and isn't merged. A vote also needs a verified email address.
- **`vote` reconciler.** It follows the post for each new voter, then sets `status.subscribed` so a follow the voter deletes isn't recreated. It records the voter's organizations and linked GitHub username in the vote's status; these Milo lookups are best effort. Votes on a deleted post are marked with `feedback.miloapis.com/orphaned-at` and deleted after 30 days unless the post returns.
- **`post-votes` reconciler.** It counts a post's votes from the cache (indexed by `spec.postRef.name`) and applies `status.votes.{total,datum,linked,trendingScore}` with server-side apply as `feedback-operator`. `github` and `githubReactions` belong to `feedback-sync`. `total = datum + github - linked`, where `linked` counts people whose 👍 comes from the GitHub account linked to their vote. Posts with a trending score are re-tallied hourly as votes age.
- **`merge` reconciler.** When `status.mergedInto` is set, it gives each voter and follower the same on the target post, unless they already have one, and deletes the old one.

## Submissions

- **Admission** (`submission` webhook): the sender's email must be verified, and each user can send at most `--submission-limit` (default 5) per `--submission-window` (default 10 minutes), counted from the cache. A `Merged` review decision must name a post that exists, isn't hidden and isn't merged (`submissionreview` webhook).
- **`submission` reconciler.** It creates each submission's `SubmissionReview`, owned by the submission so Milo's garbage collector deletes it with the submission, and sets the phase to `PendingReview`.
- **`submission-review` reconciler.** It carries out the staff decision:
  - `Rejected` copies the reason to the submission.
  - `Merged` has the submitter follow (`reason: merged`) and vote on the post.
  - `Approved` waits until feedback-sync creates the GitHub issue and syncs a post annotated `feedback.miloapis.com/submission: <name>`. It then links the submission to that post and has the submitter follow (`reason: submitted`) and vote.
  - The `Applied` condition and `status.applied` record when the decision has been fully carried out.

## Attachments

CRDs can't take file uploads, so `feedback uploads` (the `feedback-uploads` Deployment) handles them. Clients use the same bearer token they use for Milo.

```
POST /users/{user}/uploads/{submissions|securityreports}?name=shot.png   (body: the file, with its Content-Type)
GET  /users/{user}/{submissions|securityreports}/{name}/attachments/{ref} (the owner)
GET  /{submissions|securityreports}/{name}/attachments/{ref}             (staff)
```

- **Checking the caller.** A Milo `TokenReview` identifies the caller. A `SubjectAccessReview` checks that they may create (to upload) or get (to download) that resource in the context the path names. In a user's context, the service also checks that the object is theirs.
- **Files accepted.** Files are type-checked against their content: PNG, JPEG, GIF, WebP, PDF, text and JSON only. The size limit is 10 MiB by default, and each user gets a per-minute upload limit.
- **Storage.** Files land under `uploads/<purpose>/<user>/` in the bucket.
- **Attaching.** The `attachments` mutating webhook checks every `uploadRef` on a new submission or report is the owner's upload for that purpose, and sets its name and type from the upload.
- **Keeping and deleting.** The operator copies attached files under `submissions/<name>/` as soon as the submission exists, and adds the `feedback.miloapis.com/attachments` finalizer. Deleting the submission deletes its files.
- **Lifecycle rule.** The bucket's lifecycle rule should delete `uploads/` after a day.
- **Configuration.** Set the bucket in the `feedback-config` ConfigMap (`attachments-bucket`). The operator and the upload service share it.

## Security reports

- **Admission** (`securityreport` webhook):
  - Credit links must be http(s), and public credit needs a name.
  - Reports with no owner (logged by the Security Team for someone without an account) need the reporter's email or GitHub username.
  - Status updates can add timeline entries but not change or remove them. A phase change needs an entry for the new phase; the `securitytimeline` webhook adds it if the update didn't. `NotApplicable` and `Duplicate` need a reason, credit rewards need an amount, and advisories need an https link.
- **`security-report` reconciler:**
  - A new report starts in `Received` with its first timeline entry.
  - It creates the report's `SecurityReportTriage` for the Security Team's private notes.
  - It copies attachments under `security-reports/<name>/` and deletes them with the report.
  - A `Fixed` or `Disclosed` report with public credit gets a `SecurityAcknowledgement` with the name, link, month and severity. It's deleted if that stops being true.
- **`move-to-security` reconciler:** carries out `MoveToSecurity` review decisions. It creates a report owned by the submitter and named after the submission, copies the attachments, then deletes the submission, which takes its review and files with it.

## Audit and activity

Milo audits feedback writes like any other CRD. Its audit policy (in `datum-cloud/infra`) logs feedback writes at `RequestResponse`, so search can index posts from the audit log. It logs `submissions`, `submissionreviews`, `securityreports`, `securityreporttriages` and `feedbackdigests`, with their subresources, at `Metadata` only: private context and vulnerability details never reach the audit log, activity or search.

`config/milo/activity` holds an `ActivityPolicy` per kind, so people's actions read as sentences in activity timelines: voting, following, sending feedback or a report, staff decisions, hiding posts and editing sources. The service's own components are `system:` users and are left out. `test/config` compiles every rule and checks that rules for metadata-only kinds never read request or response bodies.

## Search

`config/milo/search` has a `ResourceIndexPolicy` for `Post`, `Release` and `ChangelogEntry`.

- **What's searchable:** title, summary or body, categories, labels and state. Derived fields (`votes`, `trendingScore`, `delivery`) aren't indexed, since they change as side effects and would go stale.
- **Hidden posts:** the policy's condition leaves them out, and hiding a post removes it from the index.
- **How clients use it:** a `ResourceSearchQuery` for keyword search, and for similar posts while someone writes feedback.
- **Never indexed:** submissions, reviews, security reports, votes and follows.
- **What it relies on:** the indexer reads objects from Milo's audit log, so it needs Milo to log feedback writes at `RequestResponse` (see Audit and activity).
- **Measuring lag:** `hack/search-lag.sh <post>` measures how long a post change takes to reach the index. Run it against staging; it hides the post briefly.

## Roles

| Role | For |
| --- | --- |
| `feedback.miloapis.com-user` | every signed-in user, in their own context, through Milo's `iam-user-self-manage` |
| `feedback.miloapis.com-public-reader` | the datum.net service account |
| `feedback.miloapis.com-viewer`, `-editor`, `-reviewer`, `-admin` | staff |
| `feedback.miloapis.com-security-team` | the Security Team only; the only role that reads every report and their triage |

The service's components (`system:feedback-operator`, `system:feedback-sync`,
`system:feedback-uploads`, all `@feedback.miloapis.com`) authenticate with
client certificates and get Kubernetes RBAC in Milo (`config/milo/rbac`).

## Layout

```
cmd/feedback/             operator, uploads and version commands
internal/attachments/     the attachments bucket
internal/controller/      reconcilers
internal/identity/        who a request comes from, from Milo's user info
internal/index/           cache field indexes shared by reconcilers and webhooks
internal/milo/            voter lookups in Milo
internal/ratelimit/       per-user token bucket
internal/uploads/         the feedback-uploads HTTP service
internal/webhook/         admission webhooks Milo calls
pkg/apis/feedback/        feedback.miloapis.com/v1alpha1 types (CRDs)
config/crd/               generated CRDs, installed in Milo
config/base/              operator and uploads Deployments, Services, leader election RBAC
config/components/        milo-integration: Milo kubeconfig, client and webhook certificates
config/milo/              applied to Milo: IAM (iam/), RBAC for the service's
                          components (rbac/), webhook config (webhook/),
                          activity rules (activity/), search index policies
                          (search/), and optional role bindings (bindings/)
config/samples/           an example of every resource
hack/search-lag.sh        measures how long a post change takes to reach search
test/config/              checks config/ against the CRDs
test/e2e/                 Chainsaw end-to-end tests in kind (see End-to-end tests)
hack/e2e/                 deploys the end-to-end environment
```

## Development

```bash
task build      # bin/feedback
task test       # unit tests, plus CRD tests against an envtest API server
task generate   # after changing types in pkg/apis: deepcopy and CRDs
task docker-build
```

### End-to-end tests

`task e2e:up` builds a kind cluster from
[datum-cloud/test-infra](https://github.com/datum-cloud/test-infra) (cert-manager,
Envoy Gateway, Flux), deploys Milo into it from a Milo checkout, and installs
feedback in that Milo. `task e2e:test` runs the [Chainsaw](https://kyverno.github.io/chainsaw/)
tests in `test/e2e/` against it. Docker, kind, kubectl, kustomize, jq and yq
must be installed.

```bash
task e2e:up MILO_DIR=../milo   # MILO_DIR defaults to a sibling ../milo
task e2e:test                  # or: task e2e:test -- test/e2e/votes
task e2e:redeploy              # rebuild feedback and restart it
task e2e:down                  # deletes the kind cluster
```

`e2e:up` runs `e2e:cluster`, `e2e:milo` (Milo's own `dev:build`, `dev:load`
and `dev:deploy`, then `e2e:milo-identities`), `e2e:build` and `e2e:deploy`;
each can be run on its own. Milo answers on https://localhost:30443.

What runs where:

- **In Milo:** the CRDs, `config/milo/rbac` and the admission webhooks
  (with the webhook CA as `caBundle`, since cert-manager can't inject into
  Milo), and the `post-ref` admission policy from `config/milo/activity`.
  `config/milo/iam` is checked with a server-side dry run only.
- **In the cluster** (`config/overlays/e2e`): the operator and
  `feedback-uploads` (one replica each), a self-signed webhook certificate
  from cert-manager, MinIO as the attachments bucket through the
  `s3-attachments` component, and in `milo-system` a stand-in for Milo's
  audit log processor that logs the audit events Milo sends it.
- **Identities:** `hack/e2e/milo-identities.sh` adds static tokens to Milo's
  token file (`config/overlays/e2e/milo/tokens.csv`) for
  `system:feedback-operator@feedback.miloapis.com`,
  `system:feedback-uploads@feedback.miloapis.com` and
  `system:feedback-sync@feedback.miloapis.com` (production uses client
  certificates for the same usernames), and for `other-user`, `limit-user`,
  `staff-user` and `security-user` besides Milo's `test-user`.

test-infra's Milo authorizes with Kubernetes RBAC, not Milo IAM. The tests
grant the IAM roles' permissions as ClusterRoles instead
(`hack/e2e/iam-rbac.sh`): `feedback.miloapis.com-user` to every signed-in
user, `-reviewer` to `staff-user` and `-security-team` to `security-user`.
RBAC can't scope a grant to a user's own context, so the tests don't cover
what Milo IAM decides by context: for example, that a user can't act in
another user's context, or read a submission's attachments in the platform
context.

| Test | What it checks |
| --- | --- |
| `votes` | owner webhook, `<post>.<user>` names, owner filtering, follows, vote totals, post named in delete audit events |
| `hidden-posts` | hidden posts invisible to users, visible to staff, no votes on them |
| `merge-chain` | votes moved along merge chains; a hidden target keeps them, with a condition and no hot loop |
| `submissions` | review created, reviewer recorded, Rejected and Approved carried out |
| `rate-limit` | 429 after the submission limit (`--submission-window=30s` here) |
| `attachments` | upload, attach, copy, download and delete through `feedback-uploads` and MinIO, called from inside the cluster |
| `security` | MoveToSecurity leaves an unrelated report with the target name alone, then moves once it's gone |
| `operator-readiness` | the operator isn't ready before its webhook server serves TLS |

Not covered: `feedback-sync` and GitHub (the tests write posts with sync's
identity), Milo IAM, activity and search (their services don't run in
test-infra), notifications and digests.

### Running the operator by hand

Run the operator against a Milo you can reach, in a container:

```bash
docker run --rm -v "$PWD:/src" -v "$HOME/.milo:/milo:ro" -w /src golang:1.27 \
  go run ./cmd/feedback operator --milo-kubeconfig=/milo/kubeconfig --webhook-cert-dir=/tmp/certs
```

## License

[AGPL-3.0](LICENSE)
