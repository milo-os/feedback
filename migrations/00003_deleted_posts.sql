-- +goose Up
-- When a post is deleted (label removed, repo made private, issue deleted)
-- its votes and subscriptions are kept for a retention period in case it
-- comes back. A row here records when it went; recreating the post removes
-- the row, and the apiserver purges votes for rows past retention.
CREATE TABLE deleted_posts (
    name       text        PRIMARY KEY,
    deleted_at timestamptz NOT NULL
);
CREATE INDEX deleted_posts_deleted_at_idx ON deleted_posts (deleted_at);

-- +goose Down
SELECT 1;
