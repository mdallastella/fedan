// Package store opens the SQLite database and applies schema migrations.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Default templates for new destinations.
const (
	DefaultTitleTemplate = "{{.Item.Title}}"
	DefaultBodyTemplate  = "{{.Feed.Title}}\n{{.Item.Link}}"
)

// ErrNoDefaultDestination is returned on first start when no default
// destination URL was provided.
var ErrNoDefaultDestination = errors.New("no default destination: set FEDAN_DEFAULT_URL on first start")

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, dbPath string) (*sql.DB, error) {
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version, err := strconv.Atoi(strings.SplitN(path.Base(f), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", f)
		}
		if version <= current {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDefaultDestination creates the default destination from url, subscribed
// to main, if the database has none. It returns the stored URL, which differs
// from url when the default destination already existed and was changed since.
func EnsureDefaultDestination(ctx context.Context, db *sql.DB, url string) (string, error) {
	var stored string
	err := db.QueryRowContext(ctx, "SELECT url FROM destinations WHERE is_default").Scan(&stored)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if url == "" {
		return "", ErrNoDefaultDestination
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	now := Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO destinations
		(name, url, is_default, title_template, body_template, created_at, updated_at)
		VALUES ('default', ?, 1, ?, ?, ?, ?)`,
		url, DefaultTitleTemplate, DefaultBodyTemplate, now, now)
	if err != nil {
		return "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO destination_tags (destination_id, tag_id)
		SELECT ?, id FROM tags WHERE name = 'main'`, id); err != nil {
		return "", err
	}
	return url, tx.Commit()
}

// Secret returns the persisted value for key, generating a random one of
// n bytes (hex-encoded) on first use.
func Secret(ctx context.Context, db *sql.DB, key string, n int) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, n)
	_, _ = rand.Read(b)
	v = hex.EncodeToString(b)
	if _, err := db.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT DO NOTHING", key, v); err != nil {
		return "", err
	}
	// Re-read in case another process won the race.
	return v, db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
}

// Now returns the current time in the stored timestamp format.
func Now() string {
	return Time(time.Now())
}

// Time formats t in the stored timestamp format.
func Time(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
