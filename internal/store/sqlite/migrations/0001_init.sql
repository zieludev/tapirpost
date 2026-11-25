-- Timestamps are unix microseconds. JSON is stored as text.

-- The name is the key everything refers to. The id only tells apart types
-- that had the same name one after the other.
CREATE TABLE content_types (
	name           TEXT PRIMARY KEY,
	id             TEXT NOT NULL UNIQUE,
	description    TEXT NOT NULL,
	fields         TEXT NOT NULL,
	schema_version INTEGER NOT NULL,
	revision       INTEGER NOT NULL,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE entries (
	id                TEXT PRIMARY KEY,
	type              TEXT NOT NULL REFERENCES content_types (name) ON DELETE CASCADE,
	data              TEXT NOT NULL,
	published_data    TEXT,
	version           INTEGER NOT NULL,
	published_version INTEGER,
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	published_at      INTEGER
) STRICT;

CREATE INDEX entries_type ON entries (type, id);

CREATE INDEX entries_published ON entries (type, published_at) WHERE published_data IS NOT NULL;

CREATE TABLE tokens (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	prefix     TEXT NOT NULL,
	scopes     TEXT NOT NULL,
	types      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER
) STRICT;
