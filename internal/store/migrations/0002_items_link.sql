-- Cross-feed dedup: an article already delivered to a destination from one
-- feed is not delivered again from another.
CREATE INDEX items_link ON items (link) WHERE link <> '';
