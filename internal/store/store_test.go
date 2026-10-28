package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnsureDefaultDestination(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)

	if _, err := EnsureDefaultDestination(ctx, db, ""); !errors.Is(err, ErrNoDefaultDestination) {
		t.Fatalf("empty url on first start: got %v", err)
	}
	if got, err := EnsureDefaultDestination(ctx, db, "logger://"); err != nil || got != "logger://" {
		t.Fatalf("create: got %q, %v", got, err)
	}
	// Later starts keep the stored URL, whatever the env says.
	if got, err := EnsureDefaultDestination(ctx, db, "ntfy://other"); err != nil || got != "logger://" {
		t.Fatalf("existing: got %q, %v", got, err)
	}
	if got, err := EnsureDefaultDestination(ctx, db, ""); err != nil || got != "logger://" {
		t.Fatalf("existing, no env: got %q, %v", got, err)
	}

	var subscribed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM destination_tags dt
		JOIN destinations d ON d.id = dt.destination_id JOIN tags t ON t.id = dt.tag_id
		WHERE d.is_default AND t.name = 'main'`).Scan(&subscribed); err != nil {
		t.Fatal(err)
	}
	if subscribed != 1 {
		t.Fatalf("default destination subscribed to main: %d", subscribed)
	}
}

func TestPermanentDefaults(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if _, err := EnsureDefaultDestination(ctx, db, "logger://"); err != nil {
		t.Fatal(err)
	}
	now := Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO destinations
		(name, url, title_template, body_template, created_at, updated_at)
		VALUES ('other', 'logger://', '', '', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{
		"DELETE FROM destinations WHERE is_default",
		"UPDATE destinations SET is_default = 0 WHERE is_default",
		"UPDATE destinations SET is_default = 1 WHERE name = 'other'",
		"DELETE FROM destination_tags WHERE destination_id = (SELECT id FROM destinations WHERE is_default)",
		"DELETE FROM tags WHERE name = 'main'",
		"UPDATE tags SET name = 'primary' WHERE name = 'main'",
		"INSERT INTO tags (name) VALUES ('Not A Slug')",
		"INSERT INTO tags (name) VALUES ('')",
	} {
		if _, err := db.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: succeeded, want error", q)
		}
	}

	// Other destinations may subscribe to main and leave it again.
	for _, q := range []string{
		"INSERT INTO destination_tags SELECT d.id, t.id FROM destinations d, tags t WHERE d.name = 'other' AND t.name = 'main'",
		"DELETE FROM destination_tags WHERE destination_id = (SELECT id FROM destinations WHERE name = 'other')",
		"DELETE FROM destinations WHERE name = 'other'",
		"INSERT INTO tags (name) VALUES ('go-releases')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	for range 2 {
		db, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	old := Time(now.Add(-8 * 24 * time.Hour))
	recent := Time(now.Add(-time.Hour))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Feed 1 fetched fine an hour ago; feed 2 has been failing for 8 days.
	exec(`INSERT INTO feeds (id, url, next_fetch_at, last_success_at, created_at, updated_at) VALUES
		(1, 'a', ?, ?, ?, ?), (2, 'b', ?, ?, ?, ?)`, recent, recent, old, old, recent, old, old, old)
	exec(`INSERT INTO seen_keys VALUES
		(1, 'gone', ?, ?),     -- absent from the feed for 8 days: pruned
		(1, 'present', ?, ?),  -- in the last fetch: kept
		(2, 'stale', ?, ?)     -- last fetch was 8 days ago, still present then: kept`,
		old, old, old, recent, old, old)
	exec(`INSERT INTO items (id, feed_id, item_key, created_at) VALUES (1, 1, 'gone', ?), (2, 1, 'present', ?)`, old, recent)
	exec(`INSERT INTO deliveries (item_id, destination_id, next_attempt_at, created_at)
		SELECT 1, id, ?, ? FROM destinations`, old, old)

	items, keys, err := Prune(ctx, db, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if items != 1 || keys != 1 {
		t.Fatalf("pruned items=%d keys=%d, want 1 and 1", items, keys)
	}
	var left int
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM seen_keys WHERE item_key IN ('present', 'stale')").Scan(&left)
	if left != 2 {
		t.Errorf("kept keys = %d, want 2", left)
	}
}

func TestParseTags(t *testing.T) {
	got, err := ParseTags("Security, main  go-releases,main")
	if err != nil || len(got) != 3 || got[0] != "go-releases" || got[1] != "main" || got[2] != "security" {
		t.Errorf("ParseTags = %v, %v", got, err)
	}
	if _, err := ParseTags("no_underscores"); err == nil {
		t.Error("underscore accepted")
	}
	if s := Slug("  Go & Rust / News!! "); s != "go-rust-news" {
		t.Errorf("Slug = %q", s)
	}
}

func TestTagSets(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if _, err := EnsureDefaultDestination(ctx, db, "logger://"); err != nil {
		t.Fatal(err)
	}
	id, err := AddFeed(ctx, db, "https://example.com/feed", "", []string{"main", "news"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddFeed(ctx, db, "https://example.com/feed", "", nil); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate feed: %v", err)
	}
	tx, _ := db.BeginTx(ctx, nil)
	if err := SetFeedTags(ctx, tx, id, []string{"security"}); err != nil {
		t.Fatal(err)
	}
	// The default destination can't lose main, even if the form omits it.
	if err := SetDestinationTags(ctx, tx, 1, []string{"security"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var feedTags, destTags string
	_ = db.QueryRowContext(ctx, `SELECT group_concat(t.name) FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id WHERE ft.feed_id = ?`, id).Scan(&feedTags)
	_ = db.QueryRowContext(ctx, `SELECT group_concat(name, ',') FROM (SELECT t.name FROM destination_tags dt JOIN tags t ON t.id = dt.tag_id WHERE dt.destination_id = 1 ORDER BY t.name)`).Scan(&destTags)
	if feedTags != "security" || destTags != "main,security" {
		t.Errorf("feed tags %q, destination tags %q", feedTags, destTags)
	}
}
