package ui

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdallastella/fedan/internal/config"
	"github.com/mdallastella/fedan/internal/fetch"
	"github.com/mdallastella/fedan/internal/send"
	"github.com/mdallastella/fedan/internal/store"
)

type env struct {
	t      *testing.T
	db     *sql.DB
	srv    *httptest.Server
	feeds  *httptest.Server
	client *http.Client
	mu     sync.Mutex
	sent   []string
}

const feedXML = `<?xml version="1.0"?><rss version="2.0"><channel><title>Example Blog</title><link>https://example.com/</link>
<item><title>Second post</title><link>https://example.com/2</link><guid>2</guid><description>&lt;p&gt;Second &lt;b&gt;summary&lt;/b&gt;&lt;/p&gt;</description></item>
<item><title>First post</title><link>https://example.com/1</link><guid>1</guid></item>
</channel></rss>`

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
	e := &env{t: t, db: db}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := send.New(db, log, func(_ context.Context, url, title, body string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.sent = append(e.sent, url+" "+title+" "+body)
		return nil
	})
	cfg := config.Config{Interval: 15 * time.Minute, MaxItemsPerFetch: 10, FetchConcurrency: 1, Retention: 168 * time.Hour, Password: "hunter2", LogFormat: "text"}
	fetcher := fetch.New(db, log, sender, fetch.Options{Interval: cfg.Interval, MaxItems: 10, Concurrency: 1, UserAgent: "test"})
	s, err := New(db, log, cfg, fetcher, sender, "test", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/blog", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`)
	})
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, feedXML) })
	e.feeds = httptest.NewServer(mux)
	t.Cleanup(e.feeds.Close)

	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar}
	return e
}

// do sends a request like a same-origin browser form and returns the final
// status and body.
func (e *env) do(method, path string, form url.Values) (int, string) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) login() {
	e.t.Helper()
	if code, body := e.do("POST", "/login", url.Values{"password": {"hunter2"}}); code != 200 || !strings.Contains(body, "Recent deliveries") {
		e.t.Fatalf("login: %d %s", code, body)
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

func TestAuth(t *testing.T) {
	e := newEnv(t)
	if _, body := e.do("GET", "/feeds", nil); !strings.Contains(body, `name="password"`) {
		t.Fatal("unauthenticated request not sent to login")
	}
	if _, body := e.do("POST", "/login", url.Values{"password": {"wrong"}}); !strings.Contains(body, "Wrong password") {
		t.Fatal("wrong password accepted")
	}
	e.login()
	if _, body := e.do("GET", "/feeds", nil); !strings.Contains(body, "Add a feed") {
		t.Fatal("logged-in request rejected")
	}
	e.do("POST", "/logout", url.Values{})
	if _, body := e.do("GET", "/feeds", nil); !strings.Contains(body, `name="password"`) {
		t.Fatal("still logged in after logout")
	}
	// A forged cookie is rejected.
	u, _ := url.Parse(e.srv.URL)
	e.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "99999999999.deadbeef"}}) //nolint:gosec // test cookie
	if _, body := e.do("GET", "/feeds", nil); !strings.Contains(body, `name="password"`) {
		t.Fatal("forged cookie accepted")
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/deliveries/retry-failed", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
}

func TestFeedFlow(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Adding a web page discovers its feed; the first fetch sends nothing.
	_, body := e.do("POST", "/feeds", url.Values{"url": {e.feeds.URL + "/blog"}, "tags": {"main News"}})
	if !strings.Contains(body, "Example Blog") || !strings.Contains(body, "Second post") {
		t.Fatalf("feed page after add: %s", body)
	}
	if n := e.count("SELECT count(*) FROM feeds WHERE url = ?", e.feeds.URL+"/feed.xml"); n != 1 {
		t.Fatal("discovered feed not stored")
	}
	if n := e.count("SELECT count(*) FROM deliveries"); n != 0 {
		t.Fatalf("first fetch queued %d deliveries", n)
	}
	if _, body := e.do("POST", "/feeds", url.Values{"url": {e.feeds.URL + "/feed.xml"}, "tags": {"main"}}); !strings.Contains(body, "already in the list") {
		t.Error("duplicate feed accepted")
	}
	if _, body := e.do("POST", "/feeds", url.Values{"url": {e.feeds.URL + "/blog"}, "tags": {"bad_tag"}}); !strings.Contains(body, "invalid tag") {
		t.Error("bad tag accepted")
	}

	if _, body := e.do("POST", "/feeds/1/send-latest", url.Values{}); !strings.Contains(body, "queued for 1 destination") {
		t.Fatalf("send latest: %s", body)
	}
	if n := e.count("SELECT count(*) FROM deliveries d JOIN items i ON i.id = d.item_id WHERE i.title = 'Second post'"); n != 1 {
		t.Fatal("latest item not queued")
	}

	// Without main, nothing routes there: the UI warns.
	if _, body := e.do("POST", "/feeds/1/tags", url.Values{"tags": {"news"}}); !strings.Contains(body, "routed nowhere") {
		t.Error("no routed-nowhere warning")
	}
	e.do("POST", "/feeds/1/disable", url.Values{})
	if n := e.count("SELECT enabled FROM feeds WHERE id = 1"); n != 0 {
		t.Error("disable did nothing")
	}
	e.do("POST", "/feeds/1/enable", url.Values{})
	if n := e.count("SELECT enabled FROM feeds WHERE id = 1"); n != 1 {
		t.Error("enable did nothing")
	}

	for _, p := range []string{"/", "/feeds", "/feeds/1", "/destinations", "/destinations/new", "/destinations/1", "/deliveries", "/settings"} {
		if code, body := e.do("GET", p, nil); code != 200 || !strings.Contains(body, "</html>") {
			t.Errorf("GET %s: %d", p, code)
		}
	}

	e.do("POST", "/feeds/1/delete", url.Values{})
	if n := e.count("SELECT count(*) FROM feeds"); n != 0 {
		t.Error("delete did nothing")
	}
}

func TestDestinations(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{
		"name": {"Phone"}, "url": {"nope://x"}, "tags": {"security"},
		"title_template": {"{{.Item.Title}}"}, "body_template": {"{{.Item.Summary}} {{.Item.Link}}"},
		"format": {"text"}, "max_length": {"0"},
	}
	if _, body := e.do("POST", "/destinations", form); !strings.Contains(body, "invalid Shoutrrr URL") {
		t.Fatalf("bad URL accepted: %s", body)
	}
	form.Set("url", "logger://")
	form.Set("body_template", "{{.Item.Nope}}")
	if _, body := e.do("POST", "/destinations", form); !strings.Contains(body, "template error") {
		t.Fatal("bad template accepted")
	}
	form.Set("body_template", "{{.Item.Summary}} {{.Item.Link}}")
	if _, body := e.do("POST", "/destinations", form); !strings.Contains(body, "Destination saved") {
		t.Fatalf("save: %s", body)
	}
	if _, body := e.do("POST", "/destinations", form); !strings.Contains(body, "already exists") {
		t.Error("duplicate name accepted")
	}
	// The URL is never sent back to the browser.
	if _, body := e.do("GET", "/destinations/2", nil); strings.Contains(body, "logger://\"") || !strings.Contains(body, "logger://•••") {
		t.Error("destination URL not masked")
	}

	// Editing with an empty URL keeps the stored one.
	form.Set("url", "")
	form.Set("name", "Phone 2")
	e.do("POST", "/destinations/2", form)
	if n := e.count("SELECT count(*) FROM destinations WHERE name = 'Phone 2' AND url = 'logger://'"); n != 1 {
		t.Error("edit lost the URL")
	}

	// Preview with sample data, then a test send through the stored URL.
	form.Set("id", "2")
	if _, body := e.do("POST", "/destinations/preview", form); !strings.Contains(body, "sample data") || !strings.Contains(body, "new item will look like") {
		t.Errorf("preview: %s", body)
	}
	if _, body := e.do("POST", "/destinations/test", form); !strings.Contains(body, "Sent") {
		t.Errorf("test send: %s", body)
	}
	if len(e.sent) != 1 || !strings.HasPrefix(e.sent[0], "logger:// Hello from fēdan") {
		t.Errorf("sent = %v", e.sent)
	}

	// The default destination keeps main and can't be deleted.
	def := url.Values{"name": {"default"}, "tags": {"other"}, "title_template": {"x"}, "body_template": {"y"}, "format": {"text"}}
	e.do("POST", "/destinations/1", def)
	if n := e.count("SELECT count(*) FROM destination_tags dt JOIN tags t ON t.id = dt.tag_id WHERE dt.destination_id = 1 AND t.name IN ('main', 'other')"); n != 2 {
		t.Errorf("default destination tags: %d of main/other", n)
	}
	e.do("POST", "/destinations/1/delete", url.Values{})
	e.do("POST", "/destinations/2/delete", url.Values{})
	if n := e.count("SELECT count(*) FROM destinations"); n != 1 {
		t.Errorf("destinations left = %d, want only the default", n)
	}
}

func TestOPML(t *testing.T) {
	e := newEnv(t)
	e.login()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "subs.opml")
	_, _ = io.WriteString(fw, `<opml version="2.0"><body><outline text="Tech News">
		<outline text="Go" xmlUrl="https://go.dev/blog/feed.atom"/>
		<outline text="Rust" xmlUrl="https://blog.rust-lang.org/feed.xml" category="lang"/>
	</outline></body></opml>`)
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/opml", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Imported 2 feeds") {
		t.Fatalf("import: %s", body)
	}
	if n := e.count(`SELECT count(*) FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id JOIN feeds f ON f.id = ft.feed_id
		WHERE f.title = 'Rust' AND t.name IN ('main', 'tech-news', 'lang')`); n != 3 {
		t.Errorf("Rust feed tags: %d of 3", n)
	}
	_, export := e.do("GET", "/opml", nil)
	if !strings.Contains(export, `xmlUrl="https://blog.rust-lang.org/feed.xml"`) || !strings.Contains(export, `category="lang,main,tech-news"`) {
		t.Errorf("export: %s", export)
	}
}
