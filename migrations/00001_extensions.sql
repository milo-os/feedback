-- +goose Up
-- pgvector backs similar-feedback matching (PostQuery.spec.similarTo).
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down
-- Migrations are forward-only; see README.md.
SELECT 1;
