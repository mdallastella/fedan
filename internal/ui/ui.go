// Package ui serves the htmx web interface.
package ui

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mdallastella/fedan/internal/config"
	"github.com/mdallastella/fedan/internal/fetch"
	"github.com/mdallastella/fedan/internal/send"
	"github.com/mdallastella/fedan/web"
)

// Server is the web UI.
type Server struct {
	db      *sql.DB
	log     *slog.Logger
	cfg     config.Config
	fetcher *fetch.Fetcher
	sender  *send.Sender
	version string
	auth    *auth
	pages   map[string]*template.Template
}

// New returns the UI server. sessionKey signs session cookies.
func New(db *sql.DB, log *slog.Logger, cfg config.Config, fetcher *fetch.Fetcher, sender *send.Sender, version string, sessionKey []byte) (*Server, error) {
	s := &Server{db: db, log: log, cfg: cfg, fetcher: fetcher, sender: sender, version: version}
	if cfg.Auth() {
		s.auth = &auth{key: sessionKey, hash: cfg.PasswordHash, password: cfg.Password}
	}
	if err := s.parseTemplates(web.Templates()); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the UI routes, with authentication and CSRF protection.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Static())))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /{$}", s.dashboard)

	mux.HandleFunc("GET /feeds", s.feeds)
	mux.HandleFunc("POST /feeds", s.addFeed)
	mux.HandleFunc("GET /feeds/{id}", s.feed)
	mux.HandleFunc("POST /feeds/{id}/tags", s.setFeedTags)
	mux.HandleFunc("POST /feeds/{id}/fetch", s.fetchFeed)
	mux.HandleFunc("POST /feeds/{id}/send-latest", s.sendLatest)
	mux.HandleFunc("POST /feeds/{id}/enable", s.enableFeed)
	mux.HandleFunc("POST /feeds/{id}/disable", s.disableFeed)
	mux.HandleFunc("POST /feeds/{id}/delete", s.deleteFeed)

	mux.HandleFunc("GET /destinations", s.destinations)
	mux.HandleFunc("GET /destinations/new", s.newDestination)
	mux.HandleFunc("POST /destinations", s.saveDestination)
	mux.HandleFunc("GET /destinations/{id}", s.editDestination)
	mux.HandleFunc("POST /destinations/{id}", s.saveDestination)
	mux.HandleFunc("POST /destinations/{id}/delete", s.deleteDestination)
	mux.HandleFunc("POST /destinations/preview", s.previewDestination)
	mux.HandleFunc("POST /destinations/test", s.testDestination)

	mux.HandleFunc("GET /deliveries", s.deliveries)
	mux.HandleFunc("POST /deliveries/{id}/retry", s.retryDelivery)
	mux.HandleFunc("POST /deliveries/retry-failed", s.retryFailed)

	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("GET /opml", s.exportOPML)
	mux.HandleFunc("POST /opml", s.importOPML)

	var h http.Handler = mux
	if s.auth != nil {
		h = s.auth.require(h)
	}
	return http.NewCrossOriginProtection().Handler(securityHeaders(h))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// page is the data every full page renders with.
type page struct {
	Title   string
	Nav     string
	Auth    bool
	Notice  string
	Error   string
	Version string
	Data    any
}

var funcs = template.FuncMap{
	"ago":    ago,
	"mask":   mask,
	"join":   strings.Join,
	"status": statusClass,
}

func (s *Server) parseTemplates(fsys fs.FS) error {
	names, err := fs.Glob(fsys, "*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, name := range names {
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(fsys, "layout.html", name)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return nil
}

// render writes a full page; with fragment set, only that named template.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, fragment string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.fail(w, fmt.Errorf("no template %s", name))
		return
	}
	p.Auth = s.auth != nil
	p.Version = s.version
	if p.Notice == "" {
		p.Notice = r.URL.Query().Get("notice")
	}
	var b bytes.Buffer
	var err error
	if fragment != "" {
		err = t.ExecuteTemplate(&b, fragment, p.Data)
	} else {
		err = t.ExecuteTemplate(&b, "layout", p)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = b.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("ui", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// redirect sends the browser to path after a POST, with an optional notice.
func redirect(w http.ResponseWriter, r *http.Request, path, notice string) {
	if notice != "" {
		path += "?notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, path, http.StatusSeeOther) //nolint:gosec // path is always an in-app path built by handlers
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// ago formats a stored timestamp relative to now, like "5m ago" or "in 2h".
func ago(ts any) string {
	var s string
	switch v := ts.(type) {
	case string:
		s = v
	case sql.NullString:
		if !v.Valid {
			return "never"
		}
		s = v.String
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "never"
	}
	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}
	var out string
	switch {
	case d < time.Minute:
		if future {
			return "in a moment"
		}
		return "just now"
	case d < time.Hour:
		out = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		out = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		out = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + out
	}
	return out + " ago"
}

// mask hides everything but the scheme of a Shoutrrr URL, which carries
// tokens.
func mask(u string) string {
	scheme, _, ok := strings.Cut(u, "://")
	if !ok {
		return "•••"
	}
	return scheme + "://•••"
}

func statusClass(s string) string {
	switch s {
	case "sent", "ok":
		return "ok"
	case "failed", "error", "disabled":
		return "bad"
	}
	return "wait"
}
