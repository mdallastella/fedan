// Package send delivers queued items to their destinations through Shoutrrr.
package send

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/format"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/mdallastella/fedan/internal/store"
)

const (
	tick        = 5 * time.Second
	batch       = 50
	sendTimeout = 30 * time.Second
	// Retries start after firstRetry and double up to maxRetry; a delivery
	// still failing giveUp after it was queued is marked failed.
	firstRetry = time.Minute
	maxRetry   = time.Hour
	giveUp     = 24 * time.Hour
	// A destination failing for this long triggers the failing alert.
	alertAfter = 5 * time.Minute
)

// Func delivers one message to a Shoutrrr URL.
type Func func(ctx context.Context, url, title, body string) error

// Shoutrrr sends through Shoutrrr.
func Shoutrrr(ctx context.Context, url, title, body string) error {
	params := types.Params{}
	if title != "" {
		params.SetTitle(title)
	}
	return shoutrrr.SendContext(ctx, url, body, &params)
}

// Validate reports whether url is a Shoutrrr URL for a known service.
func Validate(url string) error {
	sender, err := shoutrrr.CreateSenderWithOptions(types.SenderOptions{}, url)
	if err != nil {
		return err
	}
	_ = sender.Close()
	return nil
}

// Sender works through the delivery outbox.
type Sender struct {
	db   *sql.DB
	log  *slog.Logger
	send Func
	now  func() time.Time
}

// New returns a Sender delivering with send.
func New(db *sql.DB, log *slog.Logger, send Func) *Sender {
	return &Sender{db: db, log: log, send: send, now: time.Now}
}

// Run sends due deliveries until ctx is cancelled.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		for {
			n, err := s.SendDue(ctx)
			if err != nil {
				s.log.Error("send due deliveries", "err", err)
			}
			if err != nil || n < batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SendDue attempts up to one batch of due deliveries and returns how many it
// attempted.
func (s *Sender) SendDue(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM deliveries
		WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY id LIMIT ?`,
		store.Time(s.now()), batch)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, id := range ids {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if err := s.deliver(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

type delivery struct {
	id        int64
	attempts  int
	createdAt time.Time
	dest      destination
	data      Data
}

type destination struct {
	id                          int64
	name, url                   string
	isDefault                   bool
	titleTemplate, bodyTemplate string
	format                      string
	maxLength                   int
	failingSince                sql.NullString
	failingAlerted              bool
}

// deliver makes one attempt at a delivery. Only database errors are returned;
// send failures are recorded on the delivery.
func (s *Sender) deliver(ctx context.Context, id int64) error {
	d, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	now := s.now()
	msg, err := Render(d.dest.titleTemplate, d.dest.bodyTemplate, d.dest.format, d.dest.maxLength, d.data)
	if err != nil {
		// A broken template won't fix itself on retry.
		return s.record(ctx, d, now, fmt.Errorf("template: %w", err), true)
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	err = s.send(sendCtx, d.dest.url, msg.Title, msg.Body)
	cancel()
	if ctx.Err() != nil {
		// Shutting down: leave the delivery for the next start.
		return nil
	}
	return s.record(ctx, d, now, err, false)
}

func (s *Sender) load(ctx context.Context, id int64) (delivery, error) {
	var d delivery
	var created string
	var published sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT dl.id, dl.attempts, dl.created_at,
			ds.id, ds.name, ds.url, ds.is_default, ds.title_template, ds.body_template, ds.format,
			ds.max_length, ds.failing_since, ds.failing_alerted,
			f.id, f.title, f.url, f.site_url,
			i.title, i.link, i.summary, i.published_at
		FROM deliveries dl
		JOIN destinations ds ON ds.id = dl.destination_id
		JOIN items i ON i.id = dl.item_id
		JOIN feeds f ON f.id = i.feed_id
		WHERE dl.id = ?`, id).Scan(&d.id, &d.attempts, &created,
		&d.dest.id, &d.dest.name, &d.dest.url, &d.dest.isDefault, &d.dest.titleTemplate, &d.dest.bodyTemplate,
		&d.dest.format, &d.dest.maxLength, &d.dest.failingSince, &d.dest.failingAlerted,
		&d.data.Feed.ID, &d.data.Feed.Title, &d.data.Feed.URL, &d.data.Feed.SiteURL,
		&d.data.Item.Title, &d.data.Item.Link, &d.data.Item.Summary, &published)
	if err != nil {
		return d, err
	}
	if d.createdAt, err = time.Parse(time.RFC3339, created); err != nil {
		return d, err
	}
	if published.Valid {
		if t, err := time.Parse(time.RFC3339, published.String); err == nil {
			d.data.Item.Published = t
		}
	}
	d.data.Tags, err = feedTags(ctx, s.db, d.data.Feed.ID)
	return d, err
}

func feedTags(ctx context.Context, db *sql.DB, feedID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.name FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id
		WHERE ft.feed_id = ? ORDER BY t.name`, feedID)
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

// record stores the outcome of an attempt and updates the destination's
// failing state, alerting on transitions.
func (s *Sender) record(ctx context.Context, d delivery, now time.Time, sendErr error, permanent bool) error {
	ts := store.Time(now)
	attempts := d.attempts + 1
	var err error
	switch {
	case sendErr == nil:
		_, err = s.db.ExecContext(ctx, `UPDATE deliveries SET status = 'sent', attempts = ?, last_error = '', sent_at = ?
			WHERE id = ?`, attempts, ts, d.id)
	case permanent || now.Sub(d.createdAt) >= giveUp:
		_, err = s.db.ExecContext(ctx, `UPDATE deliveries SET status = 'failed', attempts = ?, last_error = ?
			WHERE id = ?`, attempts, sendErr.Error(), d.id)
		s.log.Warn("delivery failed", "delivery", d.id, "destination", d.dest.name, "attempts", attempts, "err", sendErr)
	default:
		_, err = s.db.ExecContext(ctx, `UPDATE deliveries SET attempts = ?, last_error = ?, next_attempt_at = ?
			WHERE id = ?`, attempts, sendErr.Error(), store.Time(now.Add(retryDelay(attempts))), d.id)
		s.log.Info("delivery will be retried", "delivery", d.id, "destination", d.dest.name, "attempts", attempts, "err", sendErr)
	}
	if err != nil {
		return err
	}
	if permanent {
		// A template error says nothing about the destination's health.
		return nil
	}
	return s.updateHealth(ctx, d.dest, now, sendErr)
}

func (s *Sender) updateHealth(ctx context.Context, dest destination, now time.Time, sendErr error) error {
	switch {
	case sendErr == nil && dest.failingSince.Valid:
		if _, err := s.db.ExecContext(ctx, "UPDATE destinations SET failing_since = NULL, failing_alerted = 0 WHERE id = ?", dest.id); err != nil {
			return err
		}
		if dest.failingAlerted {
			s.log.Info("destination recovered", "destination", dest.name)
			s.Alert(ctx, "fēdan: destination recovered", fmt.Sprintf("Deliveries to %q work again.", dest.name))
		}
	case sendErr != nil && !dest.failingSince.Valid:
		_, err := s.db.ExecContext(ctx, "UPDATE destinations SET failing_since = ? WHERE id = ?", store.Time(now), dest.id)
		return err
	case sendErr != nil && !dest.failingAlerted:
		since, err := time.Parse(time.RFC3339, dest.failingSince.String)
		if err != nil || now.Sub(since) < alertAfter {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE destinations SET failing_alerted = 1 WHERE id = ?", dest.id); err != nil {
			return err
		}
		s.log.Warn("destination failing", "destination", dest.name, "since", dest.failingSince.String, "err", sendErr)
		if !dest.isDefault {
			s.Alert(ctx, "fēdan: destination failing",
				fmt.Sprintf("Deliveries to %q have been failing since %s.\nLast error: %v", dest.name, dest.failingSince.String, sendErr))
		}
	}
	return nil
}

func retryDelay(attempts int) time.Duration {
	if attempts > 7 {
		return maxRetry
	}
	return min(firstRetry<<(attempts-1), maxRetry)
}

// Alert sends a plain-text message to the default destination, bypassing the
// outbox. Failures are only logged.
func (s *Sender) Alert(ctx context.Context, title, body string) {
	var url string
	if err := s.db.QueryRowContext(ctx, "SELECT url FROM destinations WHERE is_default").Scan(&url); err != nil {
		s.log.Error("alert: no default destination", "err", err, "title", title)
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.send(sendCtx, url, title, body); err != nil {
		s.log.Error("alert not delivered", "err", err, "title", title)
	}
}

// Data is what destination templates see.
type Data struct {
	Feed struct {
		ID                  int64
		Title, URL, SiteURL string
	}
	Item struct {
		Title, Link, Summary string
		Published            time.Time
	}
	Tags []string
}

// Message is a rendered notification.
type Message struct {
	Title, Body string
}

// Render executes a destination's templates. The item summary, HTML in feeds,
// is converted to the destination format first, and the body is truncated to
// maxLength runes (0 means no limit).
func Render(titleTmpl, bodyTmpl, outFormat string, maxLength int, data Data) (Message, error) {
	summary, err := format.ConvertFormat(data.Item.Summary, "html", outFormat)
	if err != nil {
		return Message{}, err
	}
	data.Item.Summary = strings.TrimSpace(summary)
	title, err := execute("title", titleTmpl, data)
	if err != nil {
		return Message{}, err
	}
	body, err := execute("body", bodyTmpl, data)
	if err != nil {
		return Message{}, err
	}
	return Message{Title: strings.TrimSpace(title), Body: truncate(strings.TrimSpace(body), maxLength)}, nil
}

func execute(name, text string, data Data) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:max(n-1, 0)]) + "…"
}

// Send delivers a message to url right away, bypassing the outbox.
func (s *Sender) Send(ctx context.Context, url string, msg Message) error {
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return s.send(sendCtx, url, msg.Title, msg.Body)
}
