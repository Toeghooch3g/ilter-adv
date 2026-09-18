-- Enable the vector extensions the semantic cache Postgres backend uses.
-- The timescale/timescaledb-ha image only preloads timescaledb and
-- timescaledb_toolkit; pgvector and pgvectorscale must be created explicitly.
-- Executed on first boot via /docker-entrypoint-initdb.d.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS vectorscale;
