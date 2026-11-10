# CLAUDE.md

Guidance for agents working in this repository.

## What this is

`feedback-apiserver`: an aggregated Kubernetes API server for
`feedback.miloapis.com/v1alpha1`, registered with Milo through an
`APIService`. It follows `milo-os/activity` and `milo-os/search`; look there
first for patterns (OpenAPI naming, registry strategies, Milo IAM config).

The design is `architecture/design/feedback/README.md` in
`datum-cloud/enhancements`. Work is tracked in epic
datum-cloud/enhancements#943.

## Rules that shape the code

- `FeedbackSource` is stored in etcd through the generic registry. Every other
  resource uses custom `rest.Storage` backed by Postgres.
- The apiserver is the only component that connects to Postgres. Side effects
  of a request (e.g. the `Subscription` created with a `Vote`) are written in
  the same transaction.
- GitHub-derived fields live in `status` and are written only by the
  `feedback-sync` service account. Staff can only patch `spec.hidden` on posts.
- `Submission` and `SecurityReport` are audited at `Metadata` only. Security
  reports are never embedded, indexed, synced or sent to the AI gateway.
- Migrations in `migrations/` are forward-only. Never edit an applied one.

## Commands

```bash
task build
task test
task generate     # deepcopy, clientset, OpenAPI; CI fails if out of date
task vet
```

Run long-lived processes (Postgres, the server) in containers, not on the host.
