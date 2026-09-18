-- +goose Up

-- Cached-token pricing on discovered models: cache-read (cached input) and
-- cache-write (Anthropic cache_creation) per-token prices, defaulting to 0
-- (unknown → billed at the normal input price).
ALTER TABLE provider_models ADD COLUMN cost_cache_read REAL NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN cost_cache_write REAL NOT NULL DEFAULT 0;

-- Cached-token counts on request records so the dashboard keeps its numbers
-- accurate when cached tokens are in play.
ALTER TABLE audit_log ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_log ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE provider_models DROP COLUMN cost_cache_write;
ALTER TABLE provider_models DROP COLUMN cost_cache_read;
ALTER TABLE audit_log DROP COLUMN cache_creation_tokens;
ALTER TABLE audit_log DROP COLUMN cached_tokens;
ALTER TABLE usage_daily DROP COLUMN cache_creation_tokens;
ALTER TABLE usage_daily DROP COLUMN cached_tokens;
