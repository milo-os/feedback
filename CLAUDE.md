# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`feedback`: the `feedback.miloapis.com/v1alpha1` CRDs, installed in Milo, and
the operator that reconciles them and serves their admission webhooks. It
follows `milo-os/billing` and `datum-cloud/compute`; look there first for
patterns (kubebuilder markers, controller-runtime, Milo integration).

The design is `architecture/design/feedback/README.md` in
`datum-cloud/enhancements`. Work is tracked in epic
datum-cloud/enhancements#943.

## Rules that shape the code

- Every resource is a cluster-scoped CRD in Milo's etcd. There is no
  database and no aggregated API server.
- User-owned kinds carry `spec.userRef`, declare
  `iam.miloapis.com/owner-field` and make it a selectable field. Milo limits
  user-context requests to the caller's objects; the admission webhook sets
  `spec.userRef` from the request. Never trust a client-supplied owner.
- Vote and Subscription names are `<post>.<user>` (`v1alpha1.OwnedName`), so
  there is one per user per post.
- Anything a submitter or reporter must not see goes on a staff-only kind
  (`SubmissionReview`, `SecurityReportTriage`), not on a field.
- Side effects are reconciled by the operator, not done in the request.
  Every reconcile step must be idempotent and keyed on deterministic names.
- GitHub-derived fields live in `status` and are written only by
  `feedback-sync`. The operator owns post `votes` and `delivery`; both use
  server-side apply with their own field manager. Staff can only patch
  `spec.hidden` on posts.
- `Submission`, `SubmissionReview`, `SecurityReport`, `SecurityReportTriage`
  and `FeedbackDigest` are audited at `Metadata` only (a rule in Milo's audit
  policy in datum-cloud/infra), so their activity rules can't read request or
  response bodies. Security reports are never indexed, synced or sent to the
  AI gateway, and have no ActivityPolicy: platform-timeline readers see every
  activity entry, whoever made it.
- A delete's audit event has a `Status` response, never the object. Activity
  rules for deletes use `objectRef` or an audit annotation (see
  `config/milo/activity/post-ref.yaml`); the activity service's CEL has no
  string functions, so names can't be split.

## Commands

```bash
task build
task test         # needs network once, to fetch envtest binaries
task generate     # deepcopy and CRDs; CI fails if out of date
task vet
```

Run long-lived processes (the operator) in containers, not on the host.
