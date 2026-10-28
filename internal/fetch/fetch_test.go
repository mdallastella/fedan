package fetch

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdallastella/fedan/internal/store"
)

type alerts struct {
	mu     sync.Mutex
	titles []string
}

func (a *alerts) Alert(_ context.Context, title, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.titles = append(a.titles, title)
}

type env struct {
	t      *testing.T
	db     *sql.DB
	f      *Fetcher
	alerts *alerts
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.EnsureDefaultDestination(ctx, db, "logger://"); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, alerts: &alerts{}, now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.f = New(db, log, e.alerts, Options{Interval: 15 * time.Minute, MaxItems: 2, Concurrency: 2, UserAgent: "fedan/test"})
	e.f.now = func() time.Time { return e.now }
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.ExecContext(e.t.Context(), q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(e.t.Context(), q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *env) strings(q string, args ...any) []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(e.t.Context(), q, args...)
	if err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// addFeed inserts a feed tagged main.
func (e *env) addFeed(url string) int64 {
	e.t.Helper()
	now := store.Time(e.now)
	res, err := e.db.ExecContext(e.t.Context(), "INSERT INTO feeds (url, next_fetch_at, created_at, updated_at) VALUES (?, ?, ?, ?)", url, now, now, now)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	e.exec("INSERT INTO feed_tags SELECT ?, id FROM tags WHERE name = 'main'", id)
	return id
}

func rss(items ...int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>Test</title><link>https://example.com/</link>`)
	// Newest first, as feeds usually are.
	for _, n := range slices.Backward(items) {
		fmt.Fprintf(&b, `<item><title>Item %d</title><link>https://example.com/%d</link><guid>id-%d</guid>
			<pubDate>%s</pubDate></item>`, n, n, n, time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC).Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func TestNewItemsAndDeliveries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := rss(1, 2, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "fedan/test" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	id := e.addFeed(srv.URL)

	// First fetch: everything is history, nothing is sent.
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*) FROM items"); n != 3 {
		t.Fatalf("items after first fetch = %d, want 3", n)
	}
	if n := e.count("SELECT count(*) FROM deliveries"); n != 0 {
		t.Fatalf("deliveries after first fetch = %d, want 0", n)
	}
	if n := e.count("SELECT count(*) FROM feeds WHERE title = 'Test' AND site_url = 'https://example.com/'"); n != 1 {
		t.Fatal("feed metadata not updated")
	}

	// Three new items with a cap of two: the newest two are queued.
	body = rss(1, 2, 3, 4, 5, 6)
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := e.strings("SELECT i.title FROM deliveries d JOIN items i ON i.id = d.item_id ORDER BY d.id")
	if strings.Join(got, ",") != "Item 5,Item 6" {
		t.Fatalf("queued = %v, want [Item 5 Item 6]", got)
	}
	if n := e.count("SELECT count(*) FROM items"); n != 6 {
		t.Fatalf("items = %d, want 6", n)
	}

	// Nothing new: nothing queued.
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*) FROM deliveries"); n != 2 {
		t.Fatalf("deliveries = %d, want 2", n)
	}
}

func TestRoutingByTag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := rss(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer srv.Close()

	now := store.Time(e.now)
	e.exec("INSERT INTO tags (name) VALUES ('security'), ('unused')")
	e.exec(`INSERT INTO destinations (name, url, title_template, body_template, created_at, updated_at)
		VALUES ('sec', 'logger://', '', '', ?, ?), ('other', 'logger://', '', '', ?, ?)`, now, now, now, now)
	// sec subscribes to both of the feed's tags: it must get each item once.
	e.exec("INSERT INTO destination_tags SELECT d.id, t.id FROM destinations d, tags t WHERE d.name = 'sec' AND t.name IN ('security', 'main')")
	e.exec("INSERT INTO destination_tags SELECT d.id, t.id FROM destinations d, tags t WHERE d.name = 'other' AND t.name = 'unused'")
	id := e.addFeed(srv.URL)
	e.exec("INSERT INTO feed_tags SELECT ?, id FROM tags WHERE name = 'security'", id)

	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	body = rss(1, 2)
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	q := "SELECT count(*) FROM deliveries d JOIN destinations ds ON ds.id = d.destination_id WHERE ds.name = ?"
	for name, want := range map[string]int{"default": 1, "sec": 1, "other": 0} {
		if n := e.count(q, name); n != want {
			t.Errorf("deliveries to %s = %d, want %d", name, n, want)
		}
	}

	// The same article from a second feed is not sent again.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer other.Close()
	id2 := e.addFeed(other.URL)
	body = rss(1)
	if err := e.f.FetchFeed(ctx, id2); err != nil {
		t.Fatal(err)
	}
	body = rss(1, 2)
	if err := e.f.FetchFeed(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if n := e.count(q, "default"); n != 1 {
		t.Errorf("deliveries to default after duplicate link = %d, want 1", n)
	}

	// Untagged feeds are fetched and stored but routed nowhere.
	e.exec("DELETE FROM feed_tags WHERE feed_id = ?", id)
	body = rss(1, 2, 3)
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT count(*) FROM items WHERE feed_id = ? AND title = 'Item 3'", id); n != 1 {
		t.Error("untagged feed item not stored")
	}
	if n := e.count("SELECT count(*) FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.title = 'Item 3'"); n != 0 {
		t.Error("untagged feed item was queued")
	}
}

func TestConditionalGet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hits304 := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			hits304++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "public, max-age=7200")
		_, _ = io.WriteString(w, rss(1))
	}))
	defer srv.Close()
	id := e.addFeed(srv.URL)

	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	var next string
	_ = e.db.QueryRowContext(e.t.Context(), "SELECT next_fetch_at FROM feeds WHERE id = ?", id).Scan(&next)
	if next < store.Time(e.now.Add(2*time.Hour)) {
		t.Errorf("next fetch %s ignores max-age", next)
	}

	e.now = e.now.Add(3 * time.Hour)
	if err := e.f.FetchFeed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if hits304 != 1 {
		t.Fatalf("304 responses = %d, want 1", hits304)
	}
	// Keys of the unchanged document count as still present.
	if n := e.count("SELECT count(*) FROM seen_keys WHERE last_seen_at = ?", store.Time(e.now)); n != 1 {
		t.Error("304 did not refresh seen keys")
	}
}

func TestFailuresAndBackoff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "86400")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	id := e.addFeed(srv.URL)
	nextAfter := func() time.Duration {
		var s string
		_ = e.db.QueryRowContext(e.t.Context(), "SELECT next_fetch_at FROM feeds WHERE id = ?", id).Scan(&s)
		next, _ := time.Parse(time.RFC3339, s)
		return next.Sub(e.now)
	}

	for i, want := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour} {
		if err := e.f.FetchFeed(ctx, id); err == nil {
			t.Fatal("want error")
		}
		if d := nextAfter(); d < want || d > want+2*time.Minute {
			t.Errorf("failure %d: next fetch in %s, want about %s", i+1, d, want)
		}
	}

	status = http.StatusTooManyRequests
	_ = e.f.FetchFeed(ctx, id)
	if d := nextAfter(); d < 24*time.Hour {
		t.Errorf("Retry-After ignored: next fetch in %s", d)
	}
	if n := e.count("SELECT error_count FROM feeds WHERE id = ?", id); n != 4 {
		t.Errorf("error_count = %d, want 4", n)
	}
}

func TestGoneAndMissing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer gone.Close()
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()

	g := e.addFeed(gone.URL)
	_ = e.f.FetchFeed(ctx, g)
	if n := e.count("SELECT enabled FROM feeds WHERE id = ?", g); n != 0 {
		t.Error("410 did not disable the feed")
	}

	m := e.addFeed(missing.URL)
	_ = e.f.FetchFeed(ctx, m)
	e.now = e.now.Add(6 * 24 * time.Hour)
	_ = e.f.FetchFeed(ctx, m)
	if n := e.count("SELECT enabled FROM feeds WHERE id = ?", m); n != 1 {
		t.Fatal("disabled after 6 days of 404")
	}
	e.now = e.now.Add(24 * time.Hour)
	_ = e.f.FetchFeed(ctx, m)
	if n := e.count("SELECT enabled FROM feeds WHERE id = ?", m); n != 0 {
		t.Error("not disabled after 7 days of 404")
	}
	if len(e.alerts.titles) != 2 {
		t.Errorf("alerts = %v, want two", e.alerts.titles)
	}
}

func TestRedirects(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, rss(1)) })
	mux.Handle("/old", http.RedirectHandler("/feed", http.StatusMovedPermanently))
	mux.Handle("/temp", http.RedirectHandler("/old", http.StatusFound))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	perm := e.addFeed(srv.URL + "/old")
	temp := e.addFeed(srv.URL + "/temp")
	for _, id := range []int64{perm, temp} {
		if err := e.f.FetchFeed(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	urlOf := func(id int64) string {
		var u string
		_ = e.db.QueryRowContext(e.t.Context(), "SELECT url FROM feeds WHERE id = ?", id).Scan(&u)
		return u
	}
	if u := urlOf(perm); u != srv.URL+"/feed" {
		t.Errorf("permanent redirect: url = %s", u)
	}
	if u := urlOf(temp); u != srv.URL+"/temp" {
		t.Errorf("chain with a temporary hop rewrote url to %s", u)
	}
	if len(e.alerts.titles) != 1 {
		t.Errorf("alerts = %v, want one", e.alerts.titles)
	}
}

func TestItemKey(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>
		<item><guid>g</guid><link>https://example.com/a</link></item>
		<item><link>https://example.com/b</link></item>
		<item><title>No id</title></item>
		</channel></rss>`
	e := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer srv.Close()
	id := e.addFeed(srv.URL)
	if err := e.f.FetchFeed(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	keys := e.strings("SELECT item_key FROM seen_keys ORDER BY item_key")
	if len(keys) != 3 || keys[0] != "guid:g" || !strings.HasPrefix(keys[1], "hash:") || keys[2] != "link:https://example.com/b" {
		t.Errorf("keys = %v", keys)
	}
}
