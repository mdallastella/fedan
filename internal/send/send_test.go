package send

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdallastella/fedan/internal/store"
)

type sent struct{ url, title, body string }

type env struct {
	t    *testing.T
	db   *sql.DB
	s    *Sender
	now  time.Time
	sent []sent
	// fail makes sends to these URLs fail.
	fail map[string]bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.EnsureDefaultDestination(ctx, db, "default://"); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), fail: map[string]bool{}}
	e.s = New(db, slog.New(slog.NewTextHandler(io.Discard, nil)), func(_ context.Context, url, title, body string) error {
		if e.fail[url] {
			return errors.New("service unavailable")
		}
		e.sent = append(e.sent, sent{url, title, body})
		return nil
	})
	e.s.now = func() time.Time { return e.now }
	return e
}

func (e *env) exec(q string, args ...any) int64 {
	e.t.Helper()
	res, err := e.db.ExecContext(e.t.Context(), q, args...)
	if err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) str(q string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.db.QueryRowContext(e.t.Context(), q, args...).Scan(&s); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return s
}

// queue creates a destination (unless it exists), a feed and an item, and a
// delivery of the item to the destination.
func (e *env) queue(destURL string) int64 {
	e.t.Helper()
	now := store.Time(e.now)
	e.exec(`INSERT INTO destinations (name, url, title_template, body_template, format, max_length, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'text', 0, ?, ?) ON CONFLICT DO NOTHING`,
		destURL, destURL, store.DefaultTitleTemplate, store.DefaultBodyTemplate, now, now)
	var feed int64
	if err := e.db.QueryRowContext(e.t.Context(), `INSERT INTO feeds (url, title, next_fetch_at, created_at, updated_at)
		VALUES (?, 'Blog', ?, ?, ?) ON CONFLICT (url) DO UPDATE SET title = 'Blog' RETURNING id`,
		"https://example.com/feed", now, now, now).Scan(&feed); err != nil {
		e.t.Fatal(err)
	}
	item := e.exec(`INSERT INTO items (feed_id, item_key, title, link, summary, created_at)
		VALUES (?, ?, 'Hello', 'https://example.com/hello', '<p>Hi <b>there</b></p>', ?)`,
		feed, now+destURL+e.now.String(), now)
	return e.exec(`INSERT INTO deliveries (item_id, destination_id, next_attempt_at, created_at)
		SELECT ?, id, ?, ? FROM destinations WHERE url = ?`, item, now, now, destURL)
}

func TestSendAndRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ok := e.queue("ok://")
	if _, err := e.s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 || e.sent[0].title != "Hello" || e.sent[0].body != "Blog\nhttps://example.com/hello" {
		t.Fatalf("sent = %+v", e.sent)
	}
	if st := e.str("SELECT status FROM deliveries WHERE id = ?", ok); st != "sent" {
		t.Fatalf("status = %s", st)
	}

	e.fail["bad://"] = true
	bad := e.queue("bad://")
	var delays []time.Duration
	for range 4 {
		if _, err := e.s.SendDue(ctx); err != nil {
			t.Fatal(err)
		}
		next, _ := time.Parse(time.RFC3339, e.str("SELECT next_attempt_at FROM deliveries WHERE id = ?", bad))
		delays = append(delays, next.Sub(e.now))
		e.now = next
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("retry delays = %v, want %v", delays, want)
		}
	}

	// After a day it gives up.
	e.now = e.now.Add(24 * time.Hour)
	if _, err := e.s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.str("SELECT status FROM deliveries WHERE id = ?", bad); st != "failed" {
		t.Fatalf("status after a day = %s", st)
	}
}

func TestFailingAlerts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alerts := func() []string {
		var titles []string
		for _, s := range e.sent {
			if s.url == "default://" {
				titles = append(titles, s.title)
			}
		}
		return titles
	}

	// A short glitch: no alerts at all.
	e.fail["x://"] = true
	e.queue("x://")
	_, _ = e.s.SendDue(ctx)
	e.now = e.now.Add(time.Minute)
	e.fail["x://"] = false
	_, _ = e.s.SendDue(ctx)
	if a := alerts(); len(a) != 0 {
		t.Fatalf("alerts after a glitch = %v", a)
	}

	// A longer outage alerts once, then once on recovery.
	e.fail["x://"] = true
	for range 5 {
		e.queue("x://")
		_, _ = e.s.SendDue(ctx)
		e.now = e.now.Add(3 * time.Minute)
	}
	if a := alerts(); len(a) != 1 || a[0] != "fēdan: destination failing" {
		t.Fatalf("alerts during outage = %v", a)
	}
	e.fail["x://"] = false
	e.now = e.now.Add(time.Hour)
	_, _ = e.s.SendDue(ctx)
	if a := alerts(); len(a) != 2 || a[1] != "fēdan: destination recovered" {
		t.Fatalf("alerts after recovery = %v", a)
	}
}

func TestBrokenTemplate(t *testing.T) {
	e := newEnv(t)
	id := e.queue("ok://")
	e.exec("UPDATE destinations SET body_template = '{{.Nope}}' WHERE url = 'ok://'")
	if _, err := e.s.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.str("SELECT status FROM deliveries WHERE id = ?", id); st != "failed" {
		t.Fatalf("status = %s, want failed without retries", st)
	}
	if e.str("SELECT coalesce(failing_since, '') FROM destinations WHERE url = 'ok://'") != "" {
		t.Error("template error marked the destination failing")
	}
}

func TestRender(t *testing.T) {
	var d Data
	d.Feed.Title = "Blog"
	d.Item.Title = "Hello"
	d.Item.Link = "https://example.com/hello"
	d.Item.Summary = "<p>Hi <b>there</b>, a long summary</p>"
	d.Tags = []string{"main", "security"}

	m, err := Render("{{.Item.Title}}", "{{.Item.Summary}} {{range .Tags}}#{{.}} {{end}}", "text", 0, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.Body, "<") || !strings.Contains(m.Body, "Hi there") || !strings.HasSuffix(m.Body, "#security") {
		t.Errorf("text body = %q", m.Body)
	}

	m, err = Render("{{.Item.Title}}", "{{.Item.Summary}}", "markdown", 0, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Body, "**there**") {
		t.Errorf("markdown body = %q", m.Body)
	}

	m, err = Render("", "{{.Item.Summary}}", "html", 12, d)
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "<p>Hi <b>th…" {
		t.Errorf("truncated body = %q", m.Body)
	}

	if _, err := Render("", "{{.Item.Missing}}", "text", 0, d); err == nil {
		t.Error("unknown field rendered without error")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("logger://"); err != nil {
		t.Errorf("logger://: %v", err)
	}
	if err := Validate("nosuchservice://x"); err == nil {
		t.Error("unknown service validated")
	}
}
