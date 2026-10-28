package ui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mdallastella/fedan/internal/store"
)

type feedRow struct {
	ID             int64
	URL, Title     string
	SiteURL        string
	Enabled        bool
	DisabledReason string
	LastFetchedAt  sql.NullString
	LastSuccessAt  sql.NullString
	NextFetchAt    string
	ErrorCount     int
	LastError      string
	Tags           []string
	Routes         int
}

// Name is the feed title, or its URL before the first fetch.
func (f feedRow) Name() string {
	if f.Title != "" {
		return f.Title
	}
	return f.URL
}

// Status is "disabled", "error" or "ok".
func (f feedRow) Status() string {
	switch {
	case !f.Enabled:
		return "disabled"
	case f.ErrorCount > 0:
		return "error"
	}
	return "ok"
}

const feedColumns = `f.id, f.url, f.title, f.site_url, f.enabled, f.disabled_reason, f.last_fetched_at,
	f.last_success_at, f.next_fetch_at, f.error_count, f.last_error,
	coalesce((SELECT group_concat(name, ' ') FROM (SELECT t.name FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id
		WHERE ft.feed_id = f.id ORDER BY t.name)), ''),
	(SELECT count(DISTINCT dt.destination_id) FROM feed_tags ft JOIN destination_tags dt ON dt.tag_id = ft.tag_id
		WHERE ft.feed_id = f.id)`

func scanFeed(sc interface{ Scan(...any) error }) (feedRow, error) {
	var f feedRow
	var tags string
	err := sc.Scan(&f.ID, &f.URL, &f.Title, &f.SiteURL, &f.Enabled, &f.DisabledReason, &f.LastFetchedAt,
		&f.LastSuccessAt, &f.NextFetchAt, &f.ErrorCount, &f.LastError, &tags, &f.Routes)
	f.Tags = strings.Fields(tags)
	return f, err
}

func (s *Server) listFeeds(ctx context.Context, where string, args ...any) ([]feedRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+feedColumns+" FROM feeds f "+where+" ORDER BY lower(coalesce(nullif(f.title, ''), f.url))", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var feeds []feedRow
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

func (s *Server) allTags(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM tags ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

type feedsData struct {
	Feeds    []feedRow
	AllTags  []string
	URL      string
	TagInput string
}

func (s *Server) feeds(w http.ResponseWriter, r *http.Request) {
	s.renderFeeds(w, r, feedsData{TagInput: store.MainTag}, "")
}

func (s *Server) renderFeeds(w http.ResponseWriter, r *http.Request, d feedsData, errMsg string) {
	var err error
	if d.Feeds, err = s.listFeeds(r.Context(), ""); err != nil {
		s.fail(w, err)
		return
	}
	if d.AllTags, err = s.allTags(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "feeds", "", page{Title: "Feeds", Nav: "feeds", Error: errMsg, Data: d})
}

func (s *Server) addFeed(w http.ResponseWriter, r *http.Request) {
	d := feedsData{URL: strings.TrimSpace(r.FormValue("url")), TagInput: r.FormValue("tags")}
	tags, err := store.ParseTags(d.TagInput)
	if err != nil {
		s.renderFeeds(w, r, d, err.Error())
		return
	}
	feedURL, title, err := s.fetcher.Discover(r.Context(), d.URL)
	if err != nil {
		s.renderFeeds(w, r, d, "Could not add the feed: "+err.Error())
		return
	}
	id, err := store.AddFeed(r.Context(), s.db, feedURL, title, tags)
	if errors.Is(err, store.ErrDuplicate) {
		s.renderFeeds(w, r, d, feedURL+" is already in the list.")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// The first fetch only records the current items as seen.
	notice := "Feed added. Its current items are marked as seen; new ones will be sent."
	if err := s.fetcher.FetchFeed(r.Context(), id); err != nil {
		notice = "Feed added, but the first fetch failed: " + err.Error()
	}
	redirect(w, r, fmt.Sprintf("/feeds/%d", id), notice)
}

type itemRow struct {
	ID          int64
	Title, Link string
	PublishedAt sql.NullString
	CreatedAt   string
	Deliveries  []deliveryRow
}

type feedData struct {
	Feed    feedRow
	Items   []itemRow
	AllTags []string
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	f, err := scanFeed(s.db.QueryRowContext(ctx, "SELECT "+feedColumns+" FROM feeds f WHERE f.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	d := feedData{Feed: f}
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, link, published_at, created_at FROM items
		WHERE feed_id = ? ORDER BY coalesce(published_at, created_at) DESC, id DESC LIMIT 50`, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	for rows.Next() {
		var it itemRow
		if err := rows.Scan(&it.ID, &it.Title, &it.Link, &it.PublishedAt, &it.CreatedAt); err != nil {
			_ = rows.Close()
			s.fail(w, err)
			return
		}
		d.Items = append(d.Items, it)
	}
	_ = rows.Close()
	for i := range d.Items {
		if d.Items[i].Deliveries, err = s.listDeliveries(ctx, "WHERE dl.item_id = ?", 0, d.Items[i].ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	if d.AllTags, err = s.allTags(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "feed", "", page{Title: f.Name(), Nav: "feeds", Data: d})
}

func (s *Server) setFeedTags(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tags, err := store.ParseTags(r.FormValue("tags"))
	if err != nil {
		redirect(w, r, fmt.Sprintf("/feeds/%d", id), err.Error())
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.SetFeedTags(r.Context(), tx, id, tags); err != nil {
		s.fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/feeds/%d", id), "Tags saved.")
}

func (s *Server) fetchFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	notice := "Fetched."
	if err := s.fetcher.FetchFeed(r.Context(), id); err != nil {
		notice = "Fetch failed: " + err.Error()
	}
	redirect(w, r, fmt.Sprintf("/feeds/%d", id), notice)
}

func (s *Server) sendLatest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var itemID int64
	err := s.db.QueryRowContext(r.Context(), `SELECT id FROM items WHERE feed_id = ?
		ORDER BY coalesce(published_at, created_at) DESC, id DESC LIMIT 1`, id).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		redirect(w, r, fmt.Sprintf("/feeds/%d", id), "This feed has no items yet.")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	n, err := store.QueueItem(r.Context(), s.db, itemID)
	if err != nil {
		s.fail(w, err)
		return
	}
	notice := fmt.Sprintf("Latest item queued for %d destination(s).", n)
	if n == 0 {
		notice = "No destination is subscribed to this feed's tags."
	}
	redirect(w, r, fmt.Sprintf("/feeds/%d", id), notice)
}

func (s *Server) enableFeed(w http.ResponseWriter, r *http.Request) {
	s.feedUpdate(w, r, `UPDATE feeds SET enabled = 1, disabled_reason = '', error_count = 0, last_error = '',
		missing_since = NULL, next_fetch_at = ?, updated_at = ? WHERE id = ?`, "Feed enabled.")
}

func (s *Server) disableFeed(w http.ResponseWriter, r *http.Request) {
	s.feedUpdate(w, r, `UPDATE feeds SET enabled = 0, disabled_reason = 'disabled from the UI',
		next_fetch_at = ?, updated_at = ? WHERE id = ?`, "Feed disabled.")
}

func (s *Server) deleteFeed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM feeds WHERE id = ?", id); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/feeds", "Feed deleted.")
}

// feedUpdate runs q with the current time (twice) and the feed id.
func (s *Server) feedUpdate(w http.ResponseWriter, r *http.Request, q, notice string) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	now := store.Now()
	if _, err := s.db.ExecContext(r.Context(), q, now, now, id); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/feeds/%d", id), notice)
}

type dashboardData struct {
	Feeds, Enabled, Erroring, Disabled, Unrouted int
	Pending, Failed                              int
	Problems                                     []feedRow
	FailingDestinations                          []destinationRow
	Recent                                       []deliveryRow
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var d dashboardData
	err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(enabled), 0),
		coalesce(sum(enabled AND error_count > 0), 0), coalesce(sum(NOT enabled), 0),
		coalesce(sum(NOT EXISTS (SELECT 1 FROM feed_tags ft JOIN destination_tags dt ON dt.tag_id = ft.tag_id WHERE ft.feed_id = feeds.id)), 0)
		FROM feeds`).Scan(&d.Feeds, &d.Enabled, &d.Erroring, &d.Disabled, &d.Unrouted)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(status = 'pending'), 0), coalesce(sum(status = 'failed'), 0)
		FROM deliveries`).Scan(&d.Pending, &d.Failed); err != nil {
		s.fail(w, err)
		return
	}
	if d.Problems, err = s.listFeeds(ctx, "WHERE NOT f.enabled OR f.error_count > 0"); err != nil {
		s.fail(w, err)
		return
	}
	if d.FailingDestinations, err = s.listDestinations(ctx, "WHERE d.failing_since IS NOT NULL"); err != nil {
		s.fail(w, err)
		return
	}
	if d.Recent, err = s.listDeliveries(ctx, "", 20); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "dashboard", "", page{Title: "Dashboard", Nav: "dashboard", Data: d})
}
