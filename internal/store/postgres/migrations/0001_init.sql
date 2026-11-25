-- Timestamps are unix microseconds. Text that is compared or sorted uses the
-- "C" collation: byte order, the same as in the SQLite backend.

-- The name is the key everything refers to. The id only tells apart types
-- that had the same name one after the other.
CREATE TABLE content_types (
	name           TEXT COLLATE "C" PRIMARY KEY,
	id             TEXT COLLATE "C" NOT NULL UNIQUE,
	description    TEXT NOT NULL,
	fields         TEXT NOT NULL,
	schema_version BIGINT NOT NULL,
	revision       BIGINT NOT NULL,
	created_at     BIGINT NOT NULL,
	updated_at     BIGINT NOT NULL
);

CREATE TABLE entries (
	id                TEXT COLLATE "C" PRIMARY KEY,
	type              TEXT COLLATE "C" NOT NULL REFERENCES content_types (name) ON DELETE CASCADE,
	data              JSONB NOT NULL,
	published_data    JSONB,
	version           BIGINT NOT NULL,
	published_version BIGINT,
	created_at        BIGINT NOT NULL,
	updated_at        BIGINT NOT NULL,
	published_at      BIGINT
);

CREATE INDEX entries_type ON entries (type, id);

CREATE INDEX entries_published ON entries (type, published_at) WHERE published_data IS NOT NULL;

CREATE TABLE tokens (
	id         TEXT COLLATE "C" PRIMARY KEY,
	name       TEXT NOT NULL,
	token_hash TEXT COLLATE "C" NOT NULL UNIQUE,
	prefix     TEXT NOT NULL,
	scopes     TEXT NOT NULL,
	types      TEXT NOT NULL,
	created_at BIGINT NOT NULL,
	expires_at BIGINT
);
