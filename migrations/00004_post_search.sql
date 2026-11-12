-- +goose Up

-- Keyword matching for PostQuery. Used when the search service isn't
-- configured, and for similarTo when the AI gateway can't embed the text.
-- Title outranks summary, which outranks the body. HTML tags in bodyHTML are
-- dropped by the english configuration.
ALTER TABLE posts ADD COLUMN search_text tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('english', coalesce(object->'status'->>'title', '')), 'A') ||
    setweight(to_tsvector('english', coalesce(object->'status'->>'summary', '')), 'B') ||
    setweight(to_tsvector('english', coalesce(object->'status'->>'bodyHTML', '')), 'C')
) STORED;
CREATE INDEX posts_search_text_idx ON posts USING gin (search_text);

-- PostQuery filters and facets on categories.
CREATE INDEX posts_categories_idx ON posts USING gin ((object->'status'->'categories'));

CREATE INDEX post_embeddings_model_idx ON post_embeddings (model);
