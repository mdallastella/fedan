-- Timestamps are RFC 3339 UTC strings.

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE tags (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
         CHECK (name <> '' AND name NOT GLOB '*[^a-z0-9-]*')
);

INSERT INTO tags (name) VALUES ('main');

CREATE TABLE feeds (
    id              INTEGER PRIMARY KEY,
    url             TEXT NOT NULL UNIQUE,
    title           TEXT NOT NULL DEFAULT '',
    site_url        TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    disabled_reason TEXT NOT NULL DEFAULT '',
    -- Conditional GET validators from the last 200 response.
    etag            TEXT NOT NULL DEFAULT '',
    last_modified   TEXT NOT NULL DEFAULT '',
    next_fetch_at   TEXT NOT NULL,
    last_fetched_at TEXT,
    last_success_at TEXT,
    error_count     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    -- Start of the current run of 404 responses.
    missing_since   TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX feeds_due ON feeds (next_fetch_at) WHERE enabled;

CREATE TABLE feed_tags (
    feed_id INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    tag_id  INTEGER NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    PRIMARY KEY (feed_id, tag_id)
);

CREATE INDEX feed_tags_tag ON feed_tags (tag_id);

CREATE TABLE destinations (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE COLLATE NOCASE,
    url            TEXT NOT NULL,
    is_default     INTEGER NOT NULL DEFAULT 0,
    title_template TEXT NOT NULL,
    body_template  TEXT NOT NULL,
    format         TEXT NOT NULL DEFAULT 'text'
                   CHECK (format IN ('text', 'markdown', 'html')),
    -- 0 means no limit.
    max_length     INTEGER NOT NULL DEFAULT 0 CHECK (max_length >= 0),
    -- Set while deliveries keep failing; drives the failing/recovered alert.
    failing_since  TEXT,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX destinations_one_default ON destinations (is_default) WHERE is_default;

CREATE TABLE destination_tags (
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    tag_id         INTEGER NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    PRIMARY KEY (destination_id, tag_id)
);

CREATE INDEX destination_tags_tag ON destination_tags (tag_id);

-- Keys of every item ever seen in a feed. Outlives items: a row is pruned only
-- once the key has been absent from the feed document for the retention
-- period, so an old entry can't come back as new.
CREATE TABLE seen_keys (
    feed_id       INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    item_key      TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    -- Last fetch whose document still contained the key.
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (feed_id, item_key)
);

CREATE INDEX seen_keys_last_seen ON seen_keys (last_seen_at);

-- Item history, pruned after the retention period.
CREATE TABLE items (
    id           INTEGER PRIMARY KEY,
    feed_id      INTEGER NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    item_key     TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    link         TEXT NOT NULL DEFAULT '',
    summary      TEXT NOT NULL DEFAULT '',
    published_at TEXT,
    created_at   TEXT NOT NULL,
    UNIQUE (feed_id, item_key)
);

CREATE INDEX items_created ON items (created_at);

-- Outbox: one row per item and destination.
CREATE TABLE deliveries (
    id              INTEGER PRIMARY KEY,
    item_id         INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    destination_id  INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error      TEXT NOT NULL DEFAULT '',
    sent_at         TEXT,
    created_at      TEXT NOT NULL,
    UNIQUE (item_id, destination_id)
);

CREATE INDEX deliveries_due ON deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX deliveries_destination ON deliveries (destination_id, status);

-- The default destination and its main subscription are permanent.

CREATE TRIGGER default_destination_no_delete
BEFORE DELETE ON destinations WHEN OLD.is_default
BEGIN
    SELECT RAISE(ABORT, 'the default destination cannot be deleted');
END;

CREATE TRIGGER default_destination_stays_default
BEFORE UPDATE OF is_default ON destinations WHEN OLD.is_default AND NOT NEW.is_default
BEGIN
    SELECT RAISE(ABORT, 'the default destination cannot be unset');
END;

CREATE TRIGGER default_destination_keeps_main
BEFORE DELETE ON destination_tags
WHEN (SELECT is_default FROM destinations WHERE id = OLD.destination_id)
 AND (SELECT name FROM tags WHERE id = OLD.tag_id) = 'main'
BEGIN
    SELECT RAISE(ABORT, 'the default destination must stay subscribed to main');
END;

CREATE TRIGGER main_tag_no_delete
BEFORE DELETE ON tags WHEN OLD.name = 'main'
BEGIN
    SELECT RAISE(ABORT, 'the main tag cannot be deleted');
END;

CREATE TRIGGER main_tag_no_rename
BEFORE UPDATE OF name ON tags WHEN OLD.name = 'main' AND NEW.name <> 'main'
BEGIN
    SELECT RAISE(ABORT, 'the main tag cannot be renamed');
END;
