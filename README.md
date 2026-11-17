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
stores attachments. Triage, notifications and digests, audit and search
config are tracked as sub-issues of the epic.

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

`config/milo/activity` holds an `ActivityPolicy` per kind, so people's actions read as sentences in activity timelines: voting, following, sending feedback, staff decisions, hiding posts and editing sources. The service's own components are `system:` users and are left out. A delete's audit event carries a `Status`, not the deleted object, so a `ValidatingAdmissionPolicy` (`post-ref.yaml`) adds the post's name to vote and subscription deletes as the audit annotation `post-ref.feedback.miloapis.com/post`, and the unvote and unfollow rules read it there. Security reports and their triage have no `ActivityPolicy`: the activity service shows every entry to anyone who can read the platform timeline, with no per-entry access check, so an entry would reveal that a report exists. `test/config` compiles every rule in the activity service's CEL environment, checks that rules never read bodies a metadata-only kind or a delete won't have, renders delete events written by an envtest API server, and checks no policy covers security reports.

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
                          activity rules (activity/), and optional role
                          bindings (bindings/)
config/samples/           an example of every resource
test/config/              checks config/ against the CRDs
```

## Development

```bash
task build      # bin/feedback
task test       # unit tests, plus CRD tests against an envtest API server
task generate   # after changing types in pkg/apis: deepcopy and CRDs
task docker-build
```

Run the operator against a Milo you can reach, in a container:

```bash
docker run --rm -v "$PWD:/src" -v "$HOME/.milo:/milo:ro" -w /src golang:1.27 \
  go run ./cmd/feedback operator --milo-kubeconfig=/milo/kubeconfig --webhook-cert-dir=/tmp/certs
```

## License

[AGPL-3.0](LICENSE)
