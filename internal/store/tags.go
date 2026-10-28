package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// MainTag is given to every new feed and routes to the default destination.
const MainTag = "main"

// ErrDuplicate is returned when a feed URL or destination name is taken.
var ErrDuplicate = errors.New("already exists")

var tagRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// ParseTags splits a comma- or space-separated tag list, lowercased and
// deduplicated. Spaces inside a tag are not allowed, so "Go Releases" becomes
// two tags; use "go-releases".
func ParseTags(s string) ([]string, error) {
	var tags []string
	for t := range strings.FieldsFuncSeq(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if !tagRe.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q: use lowercase letters, digits and -", t)
		}
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	slices.Sort(tags)
	return tags, nil
}

// Slug turns free text, such as an OPML folder name, into a tag.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func tagIDs(ctx context.Context, tx *sql.Tx, tags []string) ([]int64, error) {
	ids := make([]int64, 0, len(tags))
	for _, t := range tags {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO tags (name) VALUES (?)
			ON CONFLICT (name) DO UPDATE SET name = excluded.name RETURNING id`, t).Scan(&id)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SetFeedTags replaces a feed's tags, creating new tags as needed.
func SetFeedTags(ctx context.Context, tx *sql.Tx, feedID int64, tags []string) error {
	ids, err := tagIDs(ctx, tx, tags)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM feed_tags WHERE feed_id = ?", feedID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO feed_tags (feed_id, tag_id) VALUES (?, ?)", feedID, id); err != nil {
			return err
		}
	}
	return nil
}

// SetDestinationTags replaces a destination's subscriptions. The default
// destination always keeps main.
func SetDestinationTags(ctx context.Context, tx *sql.Tx, destID int64, tags []string) error {
	var isDefault bool
	if err := tx.QueryRowContext(ctx, "SELECT is_default FROM destinations WHERE id = ?", destID).Scan(&isDefault); err != nil {
		return err
	}
	if isDefault && !slices.Contains(tags, MainTag) {
		tags = append(tags, MainTag)
	}
	ids, err := tagIDs(ctx, tx, tags)
	if err != nil {
		return err
	}
	keep := make([]any, 0, len(ids)+1)
	keep = append(keep, destID)
	for _, id := range ids {
		keep = append(keep, id)
		if _, err := tx.ExecContext(ctx, "INSERT INTO destination_tags (destination_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING", destID, id); err != nil {
			return err
		}
	}
	// Delete the rest; deleting and reinserting main would trip its trigger.
	q := "DELETE FROM destination_tags WHERE destination_id = ?"
	if len(ids) > 0 {
		q += " AND tag_id NOT IN (?" + strings.Repeat(", ?", len(ids)-1) + ")"
	}
	_, err = tx.ExecContext(ctx, q, keep...)
	return err
}

// AddFeed inserts a feed due for fetching now, with the given tags.
func AddFeed(ctx context.Context, db *sql.DB, url, title string, tags []string) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := Now()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO feeds (url, title, next_fetch_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (url) DO NOTHING RETURNING id`, url, title, now, now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}
	if err := SetFeedTags(ctx, tx, id, tags); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// QueueItem queues an item for every destination its feed routes to, again
// if it was delivered before. It returns the number of deliveries queued.
func QueueItem(ctx context.Context, db *sql.DB, itemID int64) (int64, error) {
	now := Now()
	res, err := db.ExecContext(ctx, `INSERT INTO deliveries (item_id, destination_id, next_attempt_at, created_at)
		SELECT DISTINCT i.id, dt.destination_id, ?, ?
		FROM items i JOIN feed_tags ft ON ft.feed_id = i.feed_id JOIN destination_tags dt ON dt.tag_id = ft.tag_id
		WHERE i.id = ?
		ON CONFLICT (item_id, destination_id) DO UPDATE SET status = 'pending', attempts = 0,
			last_error = '', sent_at = NULL, next_attempt_at = excluded.next_attempt_at, created_at = excluded.created_at`,
		now, now, itemID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryDelivery puts a failed delivery back in the queue with a fresh retry
// window.
func RetryDelivery(ctx context.Context, db *sql.DB, id int64) error {
	now := Now()
	_, err := db.ExecContext(ctx, `UPDATE deliveries SET status = 'pending', attempts = 0, last_error = '',
		next_attempt_at = ?, created_at = ? WHERE id = ? AND status = 'failed'`, now, now, id)
	return err
}
