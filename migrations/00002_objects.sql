-- +goose Up
-- One table per kind. Every table has the same leading columns, which the
-- generic store in internal/storage/pg reads and writes:
--
--   scope_type, scope_name  who owns the object: ('Platform', '') or ('User', <user UID>)
--   name                    metadata.name, unique within a scope
--   uid                     metadata.uid
--   resource_version        from resource_version_seq, bumped on every write
--   labels                  metadata.labels, for label selectors
--   object                  the full object as JSON, without resourceVersion
--
-- The remaining columns are copied out of the object on every write, for
-- field selectors, sorting and joins. The object column is the source of truth.

CREATE SEQUENCE resource_version_seq;

CREATE TABLE posts (
    scope_type          text        NOT NULL,
    scope_name          text        NOT NULL,
    name                text        NOT NULL,
    uid                 uuid        NOT NULL UNIQUE,
    resource_version    bigint      NOT NULL,
    created_at          timestamptz NOT NULL,
    labels              jsonb       NOT NULL DEFAULT '{}',
    object              jsonb       NOT NULL,
    hidden              boolean     NOT NULL DEFAULT false,
    state               text        NOT NULL DEFAULT '',
    github_org          text        NOT NULL DEFAULT '',
    github_repository   text        NOT NULL DEFAULT '',
    github_number       integer     NOT NULL DEFAULT 0,
    github_created_at   timestamptz,
    release             text        NOT NULL DEFAULT '',
    votes_total         integer     NOT NULL DEFAULT 0,
    trending_score      double precision NOT NULL DEFAULT 0,
    delivery_stage      text        NOT NULL DEFAULT '',
    delivery_changed_at timestamptz,
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX posts_state_idx ON posts (state) WHERE NOT hidden;
CREATE INDEX posts_github_org_idx ON posts (github_org);
CREATE INDEX posts_release_idx ON posts (release) WHERE release <> '';
CREATE INDEX posts_votes_total_idx ON posts (votes_total DESC, name);
CREATE INDEX posts_trending_idx ON posts (trending_score DESC, name);
CREATE INDEX posts_github_created_idx ON posts (github_created_at DESC NULLS LAST, name);
CREATE INDEX posts_delivery_changed_idx ON posts (delivery_changed_at DESC NULLS LAST, name);

-- Votes and subscriptions are in the user scope: scope_name is the user UID
-- and name is the post name. Rows outlive their post so votes survive a post
-- that is removed and later returns.
CREATE TABLE votes (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    post_name        text        NOT NULL,
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX votes_post_idx ON votes (post_name, created_at);

CREATE TABLE subscriptions (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    post_name        text        NOT NULL,
    reason           text        NOT NULL DEFAULT '',
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX subscriptions_post_idx ON subscriptions (post_name);

-- Submissions and security reports are created in the user scope. Staff list
-- them across users, so phase indexes don't lead with the scope.
CREATE TABLE submissions (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    kind             text        NOT NULL DEFAULT '',
    phase            text        NOT NULL DEFAULT '',
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX submissions_phase_idx ON submissions (phase, created_at);
-- Names are generated, and staff fetch them by name across users, so they
-- must be unique across scopes too.
CREATE UNIQUE INDEX submissions_name_key ON submissions (name);

CREATE TABLE security_reports (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    phase            text        NOT NULL DEFAULT '',
    severity         text        NOT NULL DEFAULT '',
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX security_reports_phase_idx ON security_reports (phase, created_at);
CREATE UNIQUE INDEX security_reports_name_key ON security_reports (name);

CREATE TABLE releases (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    shipped          boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (scope_type, scope_name, name)
);

CREATE TABLE changelog_entries (
    scope_type       text        NOT NULL,
    scope_name       text        NOT NULL,
    name             text        NOT NULL,
    uid              uuid        NOT NULL UNIQUE,
    resource_version bigint      NOT NULL,
    created_at       timestamptz NOT NULL,
    labels           jsonb       NOT NULL DEFAULT '{}',
    object           jsonb       NOT NULL,
    published_at     timestamptz,
    PRIMARY KEY (scope_type, scope_name, name)
);
CREATE INDEX changelog_entries_published_idx ON changelog_entries (published_at DESC NULLS LAST, name);

-- Post embeddings for PostQuery.spec.similarTo. The dimension is left open
-- until the AI gateway's embedding model is chosen; the ANN index is added
-- with it.
CREATE TABLE post_embeddings (
    post_name    text        PRIMARY KEY,
    model        text        NOT NULL,
    content_hash text        NOT NULL,
    embedding    vector      NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Pending digest events per follower. feedback-controller appends through
-- the API and marks rows sent when the daily digest goes out.
CREATE TABLE digest_events (
    id         bigserial   PRIMARY KEY,
    user_uid   text        NOT NULL,
    post_name  text        NOT NULL DEFAULT '',
    event      text        NOT NULL,
    detail     jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz
);
CREATE INDEX digest_events_pending_idx ON digest_events (user_uid, created_at) WHERE sent_at IS NULL;

-- +goose Down
-- Migrations are forward-only; see README.md.
SELECT 1;
