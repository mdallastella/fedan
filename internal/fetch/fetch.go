// Package fetch polls feeds, records new items and queues their deliveries.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/mdallastella/fedan/internal/store"
)

const (
	tick         = 30 * time.Second
	timeout      = 30 * time.Second
	maxBody      = 10 << 20
	maxRedirects = 5
	maxBackoff   = 6 * time.Hour
	// A feed answering 404 for this long is disabled.
	missingLimit = 7 * 24 * time.Hour
)

// Alerter delivers operational warnings to the user.
type Alerter interface {
	Alert(ctx context.Context, title, body string)
}

// Options configures a Fetcher.
type Options struct {
	Interval    time.Duration
	MaxItems    int
	Concurrency int
	UserAgent   string
}

// Fetcher polls due feeds.
type Fetcher struct {
	db     *sql.DB
	log    *slog.Logger
	alert  Alerter
	opts   Options
	client *http.Client
	now    func() time.Time
}

// New returns a Fetcher.
func New(db *sql.DB, log *slog.Logger, alert Alerter, opts Options) *Fetcher {
	f := &Fetcher{db: db, log: log, alert: alert, opts: opts, now: time.Now}
	f.client = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if r, ok := req.Context().Value(redirectsKey{}).(*redirects); ok {
				r.hops++
				if s := req.Response.StatusCode; s != http.StatusMovedPermanently && s != http.StatusPermanentRedirect {
					r.temporary = true
				}
			}
			return nil
		},
	}
	return f
}

// Run fetches due feeds until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		f.fetchDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *Fetcher) fetchDue(ctx context.Context) {
	rows, err := f.db.QueryContext(ctx,
		"SELECT id FROM feeds WHERE enabled AND next_fetch_at <= ? ORDER BY next_fetch_at",
		store.Time(f.now()))
	if err != nil {
		f.log.Error("list due feeds", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			f.log.Error("list due feeds", "err", err)
			break
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		f.log.Error("list due feeds", "err", err)
	}
	_ = rows.Close()

	sem := make(chan struct{}, f.opts.Concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := f.FetchFeed(ctx, id); err != nil {
				f.log.Warn("fetch failed", "feed", id, "err", err)
			}
		})
	}
	wg.Wait()
}

type feed struct {
	id            int64
	url           string
	title         string
	etag          string
	lastModified  string
	lastSuccessAt sql.NullString
	errorCount    int
	missingSince  sql.NullString
}

type redirectsKey struct{}

// redirects records a redirect chain and whether any hop was temporary.
type redirects struct {
	hops      int
	temporary bool
}

// FetchFeed fetches one feed now, whatever its schedule. The returned error
// describes a failed fetch, which is also recorded on the feed.
func (f *Fetcher) FetchFeed(ctx context.Context, id int64) error {
	var fd feed
	err := f.db.QueryRowContext(ctx, `SELECT id, url, title, etag, last_modified, last_success_at, error_count, missing_since
		FROM feeds WHERE id = ?`, id).Scan(&fd.id, &fd.url, &fd.title, &fd.etag, &fd.lastModified,
		&fd.lastSuccessAt, &fd.errorCount, &fd.missingSince)
	if err != nil {
		return err
	}
	now := f.now()

	rd := &redirects{}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, redirectsKey{}, rd), http.MethodGet, fd.url, nil)
	if err != nil {
		return f.fail(ctx, fd, now, err, 0)
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/xml;q=0.9, */*;q=0.8")
	if fd.etag != "" {
		req.Header.Set("If-None-Match", fd.etag)
	}
	if fd.lastModified != "" {
		req.Header.Set("If-Modified-Since", fd.lastModified)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return f.fail(ctx, fd, now, err, 0)
	}
	defer func() { _ = resp.Body.Close() }()

	switch s := resp.StatusCode; {
	case s == http.StatusNotModified:
		return f.notModified(ctx, fd, now, resp)
	case s >= 200 && s < 300:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return f.fail(ctx, fd, now, err, 0)
		}
		if len(body) > maxBody {
			return f.fail(ctx, fd, now, fmt.Errorf("feed larger than %d bytes", maxBody), 0)
		}
		parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
		if err != nil {
			return f.fail(ctx, fd, now, fmt.Errorf("parse: %w", err), 0)
		}
		moved := ""
		if u := resp.Request.URL.String(); rd.hops > 0 && !rd.temporary && u != fd.url {
			moved = u
		}
		return f.update(ctx, fd, now, resp, parsed, moved)
	case s == http.StatusGone:
		return f.disable(ctx, fd, now, "the server answered 410 Gone")
	case s == http.StatusNotFound:
		if fd.missingSince.Valid {
			since, err := time.Parse(time.RFC3339, fd.missingSince.String)
			if err == nil && now.Sub(since) >= missingLimit {
				return f.disable(ctx, fd, now, "the server answered 404 Not Found since "+fd.missingSince.String)
			}
		} else if _, err := f.db.ExecContext(ctx, "UPDATE feeds SET missing_since = ? WHERE id = ?", store.Time(now), fd.id); err != nil {
			return err
		}
		return f.fail(ctx, fd, now, errors.New(resp.Status), 0)
	default:
		return f.fail(ctx, fd, now, errors.New(resp.Status), retryAfter(resp.Header.Get("Retry-After"), now))
	}
}

// update records a successful fetch: feed metadata, seen keys, new items and
// their deliveries.
func (f *Fetcher) update(ctx context.Context, fd feed, now time.Time, resp *http.Response, parsed *gofeed.Feed, moved string) error {
	ts := store.Time(now)
	first := !fd.lastSuccessAt.Valid

	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if moved != "" {
		var taken bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM feeds WHERE url = ? AND id <> ?)", moved, fd.id).Scan(&taken); err != nil {
			return err
		}
		if taken {
			f.log.Warn("feed moved to a URL another feed already uses; keeping the old URL", "feed", fd.id, "from", fd.url, "to", moved)
			moved = ""
		}
	}
	title := fd.title
	if t := strings.TrimSpace(parsed.Title); t != "" {
		title = t
	}
	url := fd.url
	if moved != "" {
		url = moved
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feeds SET url = ?, title = ?, site_url = ?, etag = ?, last_modified = ?,
		next_fetch_at = ?, last_fetched_at = ?, last_success_at = ?, error_count = 0, last_error = '',
		missing_since = NULL, updated_at = ? WHERE id = ?`,
		url, title, parsed.Link, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"),
		store.Time(f.next(now, maxAge(resp.Header.Get("Cache-Control")))), ts, ts, ts, fd.id); err != nil {
		return err
	}

	fresh, err := markSeen(ctx, tx, fd.id, ts, parsed.Items)
	if err != nil {
		return err
	}
	// Queue only the newest items; older ones are kept as history.
	queue := fresh
	if first {
		queue = nil
	} else if len(queue) > f.opts.MaxItems {
		f.log.Warn("too many new items, queueing only the newest", "feed", fd.id, "new", len(fresh), "queued", f.opts.MaxItems)
		queue = queue[len(queue)-f.opts.MaxItems:]
	}
	queued := make(map[*gofeed.Item]bool, len(queue))
	for _, it := range queue {
		queued[it] = true
	}
	for _, it := range fresh {
		id, err := insertItem(ctx, tx, fd.id, ts, it)
		if err != nil {
			return err
		}
		if queued[it] {
			if err := enqueue(ctx, tx, fd.id, id, it.Link, ts); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if len(fresh) > 0 {
		f.log.Info("fetched", "feed", fd.id, "new", len(fresh), "queued", len(queue))
	}
	if moved != "" {
		f.log.Warn("feed moved permanently", "feed", fd.id, "from", fd.url, "to", moved)
		f.alert.Alert(ctx, "fēdan: feed moved",
			fmt.Sprintf("%s moved permanently.\nOld URL: %s\nNew URL: %s", feedName(title, url), fd.url, moved))
	}
	return nil
}

// markSeen records every key in the document as seen now and returns the
// items not seen before, oldest first.
func markSeen(ctx context.Context, tx *sql.Tx, feedID int64, ts string, items []*gofeed.Item) ([]*gofeed.Item, error) {
	var fresh []*gofeed.Item
	keys := make(map[string]bool, len(items))
	for _, it := range items {
		k := itemKey(it)
		if keys[k] {
			continue
		}
		keys[k] = true
		res, err := tx.ExecContext(ctx, `INSERT INTO seen_keys (feed_id, item_key, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, feedID, k, ts, ts)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			fresh = append(fresh, it)
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE seen_keys SET last_seen_at = ? WHERE feed_id = ? AND item_key = ?", ts, feedID, k); err != nil {
			return nil, err
		}
	}
	// Feeds list newest first; order by date where every item has one.
	slices.Reverse(fresh)
	if !slices.ContainsFunc(fresh, func(it *gofeed.Item) bool { return itemTime(it) == nil }) {
		slices.SortStableFunc(fresh, func(a, b *gofeed.Item) int { return itemTime(a).Compare(*itemTime(b)) })
	}
	return fresh, nil
}

func insertItem(ctx context.Context, tx *sql.Tx, feedID int64, ts string, it *gofeed.Item) (int64, error) {
	var published sql.NullString
	if t := itemTime(it); t != nil {
		published = sql.NullString{String: store.Time(*t), Valid: true}
	}
	summary := it.Description
	if summary == "" {
		summary = it.Content
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO items (feed_id, item_key, title, link, summary, published_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (feed_id, item_key) DO UPDATE SET title = excluded.title
		RETURNING id`,
		feedID, itemKey(it), strings.TrimSpace(it.Title), it.Link, summary, published, ts).Scan(&id)
	return id, err
}

// enqueue creates a delivery for every destination subscribed to any of the
// feed's tags, skipping destinations that already received the same link.
func enqueue(ctx context.Context, tx *sql.Tx, feedID, itemID int64, link, ts string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO deliveries (item_id, destination_id, next_attempt_at, created_at)
		SELECT DISTINCT ?, dt.destination_id, ?, ?
		FROM feed_tags ft JOIN destination_tags dt ON dt.tag_id = ft.tag_id
		WHERE ft.feed_id = ?
		  AND (? = '' OR NOT EXISTS (
		      SELECT 1 FROM deliveries d JOIN items i ON i.id = d.item_id
		      WHERE d.destination_id = dt.destination_id AND i.link = ?))
		ON CONFLICT DO NOTHING`,
		itemID, ts, ts, feedID, link, link)
	return err
}

// notModified records a 304: the document still holds the keys seen on the
// last successful fetch.
func (f *Fetcher) notModified(ctx context.Context, fd feed, now time.Time, resp *http.Response) error {
	ts := store.Time(now)
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE feeds SET next_fetch_at = ?, last_fetched_at = ?, last_success_at = ?,
		error_count = 0, last_error = '', missing_since = NULL WHERE id = ?`,
		store.Time(f.next(now, maxAge(resp.Header.Get("Cache-Control")))), ts, ts, fd.id); err != nil {
		return err
	}
	if fd.lastSuccessAt.Valid {
		if _, err := tx.ExecContext(ctx, "UPDATE seen_keys SET last_seen_at = ? WHERE feed_id = ? AND last_seen_at = ?",
			ts, fd.id, fd.lastSuccessAt.String); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// fail records a failed fetch and schedules a retry with exponential backoff,
// or later if the server asked for it.
func (f *Fetcher) fail(ctx context.Context, fd feed, now time.Time, cause error, retryAfter time.Duration) error {
	backoff := maxBackoff
	if fd.errorCount < 16 {
		backoff = min(f.opts.Interval<<fd.errorCount, maxBackoff)
	}
	next := f.next(now, max(backoff, retryAfter))
	if _, err := f.db.ExecContext(ctx, `UPDATE feeds SET next_fetch_at = ?, last_fetched_at = ?,
		error_count = error_count + 1, last_error = ? WHERE id = ?`,
		store.Time(next), store.Time(now), cause.Error(), fd.id); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (f *Fetcher) disable(ctx context.Context, fd feed, now time.Time, reason string) error {
	if _, err := f.db.ExecContext(ctx, `UPDATE feeds SET enabled = 0, disabled_reason = ?, last_fetched_at = ?,
		last_error = ?, updated_at = ? WHERE id = ?`,
		reason, store.Time(now), reason, store.Time(now), fd.id); err != nil {
		return err
	}
	f.log.Warn("feed disabled", "feed", fd.id, "url", fd.url, "reason", reason)
	f.alert.Alert(ctx, "fēdan: feed disabled",
		fmt.Sprintf("%s was disabled: %s.\nURL: %s", feedName(fd.title, fd.url), reason, fd.url))
	return errors.New(reason)
}

// next returns when to fetch again: after at least the interval (or wait, if
// longer), plus up to 10% jitter.
func (f *Fetcher) next(now time.Time, wait time.Duration) time.Time {
	wait = max(wait, f.opts.Interval)
	return now.Add(wait + rand.N(f.opts.Interval/10+1)) //nolint:gosec // jitter needs no crypto randomness
}

// itemKey identifies an item within its feed: GUID (or Atom id), else link,
// else a hash of title and publication date.
func itemKey(it *gofeed.Item) string {
	if g := strings.TrimSpace(it.GUID); g != "" {
		return "guid:" + g
	}
	if l := strings.TrimSpace(it.Link); l != "" {
		return "link:" + l
	}
	h := sha256.Sum256([]byte(it.Title + "|" + it.Published))
	return "hash:" + hex.EncodeToString(h[:])
}

func itemTime(it *gofeed.Item) *time.Time {
	if it.PublishedParsed != nil {
		return it.PublishedParsed
	}
	return it.UpdatedParsed
}

func feedName(title, url string) string {
	if title != "" {
		return fmt.Sprintf("%q", title)
	}
	return url
}

// maxAge parses the max-age directive of a Cache-Control header.
func maxAge(cc string) time.Duration {
	for d := range strings.SplitSeq(cc, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(d)), "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 0
}

// retryAfter parses a Retry-After header, in seconds or as an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}
