-- +goose Up

-- Rename the model quality column tier -> category (user decision: full-stack
-- rename; "tier" now only refers to provider service_tier and smartrouter
-- complexity tiers, which are different concepts).
ALTER TABLE provider_models RENAME COLUMN tier TO category;

-- +goose Down

ALTER TABLE provider_models RENAME COLUMN category TO tier;
