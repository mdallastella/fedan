package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
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
