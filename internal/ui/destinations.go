package ui

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdallastella/fedan/internal/send"
	"github.com/mdallastella/fedan/internal/store"
)

type destinationRow struct {
	ID                          int64
	Name, URL                   string
	IsDefault                   bool
	TitleTemplate, BodyTemplate string
	Format                      string
	MaxLength                   int
	FailingSince                sql.NullString
	Tags                        []string
	Sent, Failed                int
}

const destinationColumns = `d.id, d.name, d.url, d.is_default, d.title_template, d.body_template, d.format,
	d.max_length, d.failing_since,
	coalesce((SELECT group_concat(name, ' ') FROM (SELECT t.name FROM destination_tags dt JOIN tags t ON t.id = dt.tag_id
		WHERE dt.destination_id = d.id ORDER BY t.name)), ''),
	(SELECT count(*) FROM deliveries WHERE destination_id = d.id AND status = 'sent'),
	(SELECT count(*) FROM deliveries WHERE destination_id = d.id AND status = 'failed')`

func scanDestination(sc interface{ Scan(...any) error }) (destinationRow, error) {
	var d destinationRow
	var tags string
	err := sc.Scan(&d.ID, &d.Name, &d.URL, &d.IsDefault, &d.TitleTemplate, &d.BodyTemplate, &d.Format,
		&d.MaxLength, &d.FailingSince, &tags, &d.Sent, &d.Failed)
	d.Tags = strings.Fields(tags)
	return d, err
}

func (s *Server) listDestinations(ctx context.Context, where string, args ...any) ([]destinationRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+destinationColumns+" FROM destinations d "+where+
		" ORDER BY d.is_default DESC, lower(d.name)", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var dests []destinationRow
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		dests = append(dests, d)
	}
	return dests, rows.Err()
}

func (s *Server) destinations(w http.ResponseWriter, r *http.Request) {
	dests, err := s.listDestinations(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "destinations", "", page{Title: "Destinations", Nav: "destinations", Data: dests})
}

type destinationForm struct {
	destinationRow
	New      bool
	TagInput string
	AllTags  []string
	Formats  []string
}

func (s *Server) renderDestination(w http.ResponseWriter, r *http.Request, f destinationForm, errMsg string) {
	var err error
	if f.AllTags, err = s.allTags(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	f.Formats = []string{"text", "markdown", "html"}
	title := "New destination"
	if !f.New {
		title = f.Name
	}
	s.render(w, r, "destination", "", page{Title: title, Nav: "destinations", Error: errMsg, Data: f})
}

func (s *Server) newDestination(w http.ResponseWriter, r *http.Request) {
	s.renderDestination(w, r, destinationForm{New: true, TagInput: "", destinationRow: destinationRow{
		TitleTemplate: store.DefaultTitleTemplate, BodyTemplate: store.DefaultBodyTemplate, Format: "text",
	}}, "")
}

func (s *Server) editDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := scanDestination(s.db.QueryRowContext(r.Context(), "SELECT "+destinationColumns+" FROM destinations d WHERE d.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderDestination(w, r, destinationForm{destinationRow: d, TagInput: strings.Join(d.Tags, " ")}, "")
}

// readForm reads and validates a destination form. The URL may be empty when
// editing, which keeps the stored one.
func readForm(r *http.Request) (destinationForm, error) {
	f := destinationForm{TagInput: r.FormValue("tags")}
	f.Name = strings.TrimSpace(r.FormValue("name"))
	f.URL = strings.TrimSpace(r.FormValue("url"))
	f.TitleTemplate = r.FormValue("title_template")
	f.BodyTemplate = r.FormValue("body_template")
	f.Format = r.FormValue("format")
	if f.Format != "text" && f.Format != "markdown" && f.Format != "html" {
		f.Format = "text"
	}
	if v := strings.TrimSpace(r.FormValue("max_length")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, errors.New("max length must be a number, 0 for no limit")
		}
		f.MaxLength = n
	}
	var err error
	if f.Tags, err = store.ParseTags(f.TagInput); err != nil {
		return f, err
	}
	if f.URL != "" {
		if err := send.Validate(f.URL); err != nil {
			return f, errors.New("invalid Shoutrrr URL: " + err.Error())
		}
	}
	if _, err := send.Render(f.TitleTemplate, f.BodyTemplate, f.Format, f.MaxLength, sampleData()); err != nil {
		return f, errors.New("template error: " + err.Error())
	}
	return f, nil
}

func (s *Server) saveDestination(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, editing := pathID(r)
	f, err := readForm(r)
	f.New = !editing
	if editing {
		f.ID = id
		_ = s.db.QueryRowContext(ctx, "SELECT is_default FROM destinations WHERE id = ?", id).Scan(&f.IsDefault)
	}
	switch {
	case err != nil:
	case f.Name == "":
		err = errors.New("the name is required")
	case f.URL == "" && !editing:
		err = errors.New("the URL is required")
	}
	if err != nil {
		f.URL = ""
		s.renderDestination(w, r, f, err.Error())
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	if editing {
		_, err = tx.ExecContext(ctx, `UPDATE destinations SET name = ?, url = coalesce(nullif(?, ''), url),
			title_template = ?, body_template = ?, format = ?, max_length = ?, updated_at = ? WHERE id = ?`,
			f.Name, f.URL, f.TitleTemplate, f.BodyTemplate, f.Format, f.MaxLength, now, id)
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO destinations
			(name, url, title_template, body_template, format, max_length, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			f.Name, f.URL, f.TitleTemplate, f.BodyTemplate, f.Format, f.MaxLength, now, now).Scan(&id)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			f.URL = ""
			s.renderDestination(w, r, f, "A destination named "+f.Name+" already exists.")
			return
		}
		s.fail(w, err)
		return
	}
	if err := store.SetDestinationTags(ctx, tx, id, f.Tags); err != nil {
		s.fail(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/destinations", "Destination saved.")
}

func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM destinations WHERE id = ? AND NOT is_default", id); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/destinations", "Destination deleted.")
}

type previewData struct {
	Message send.Message
	Source  string
	Error   string
	Sent    bool
}

// previewItem picks the newest item from a feed sharing a tag with the form,
// else the newest item, else made-up sample data.
func (s *Server) previewItem(ctx context.Context, tags []string) (send.Data, string) {
	if len(tags) > 0 {
		if d, ok := s.latestItem(ctx, tags); ok {
			return d, "the latest item of " + d.Feed.Title
		}
	}
	if d, ok := s.latestItem(ctx, nil); ok {
		return d, "the latest item of " + d.Feed.Title
	}
	return sampleData(), "sample data"
}

// latestItem returns the newest item of the feeds carrying any of tags, or
// of any feed when tags is empty.
func (s *Server) latestItem(ctx context.Context, tags []string) (send.Data, bool) {
	q := `SELECT f.id, f.title, f.url, f.site_url, i.title, i.link, i.summary, coalesce(i.published_at, i.created_at)
		FROM items i JOIN feeds f ON f.id = i.feed_id`
	var args []any
	if len(tags) > 0 {
		q += ` WHERE i.feed_id IN (SELECT ft.feed_id FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id
			WHERE t.name IN (?` + strings.Repeat(", ?", len(tags)-1) + `))`
		for _, t := range tags {
			args = append(args, t)
		}
	}
	var d send.Data
	var published string
	err := s.db.QueryRowContext(ctx, q+" ORDER BY i.id DESC LIMIT 1", args...).Scan(&d.Feed.ID, &d.Feed.Title,
		&d.Feed.URL, &d.Feed.SiteURL, &d.Item.Title, &d.Item.Link, &d.Item.Summary, &published)
	if err != nil {
		return d, false
	}
	d.Item.Published, _ = time.Parse(time.RFC3339, published)
	d.Tags, err = s.feedTags(ctx, d.Feed.ID)
	return d, err == nil
}

func (s *Server) feedTags(ctx context.Context, feedID int64) ([]string, error) {
	var tags string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(group_concat(name, ' '), '') FROM (SELECT t.name
		FROM feed_tags ft JOIN tags t ON t.id = ft.tag_id WHERE ft.feed_id = ? ORDER BY t.name)`, feedID).Scan(&tags)
	return strings.Fields(tags), err
}

func sampleData() send.Data {
	var d send.Data
	d.Feed.Title = "Example Blog"
	d.Feed.URL = "https://example.com/feed.xml"
	d.Feed.SiteURL = "https://example.com/"
	d.Item.Title = "Hello from fēdan"
	d.Item.Link = "https://example.com/hello"
	d.Item.Summary = "<p>This is what a <b>new item</b> will look like.</p>"
	d.Item.Published = time.Now()
	d.Tags = []string{"main"}
	return d
}

func (s *Server) previewDestination(w http.ResponseWriter, r *http.Request) {
	s.preview(w, r, false)
}

func (s *Server) testDestination(w http.ResponseWriter, r *http.Request) {
	s.preview(w, r, true)
}

// preview renders the form's templates against a real item and, for a test,
// sends the result to the form's URL (or the stored one when left empty).
func (s *Server) preview(w http.ResponseWriter, r *http.Request, sendIt bool) {
	ctx := r.Context()
	var p previewData
	f, err := readForm(r)
	if err != nil {
		p.Error = err.Error()
		s.render(w, r, "destination", "preview", page{Data: p})
		return
	}
	data, source := s.previewItem(ctx, f.Tags)
	p.Source = source
	p.Message, err = send.Render(f.TitleTemplate, f.BodyTemplate, f.Format, f.MaxLength, data)
	if err != nil {
		p.Error = err.Error()
	}
	if sendIt && p.Error == "" {
		url := f.URL
		if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); url == "" && err == nil {
			_ = s.db.QueryRowContext(ctx, "SELECT url FROM destinations WHERE id = ?", id).Scan(&url)
		}
		if url == "" {
			p.Error = "enter a URL to send a test"
		} else if err := s.sender.Send(ctx, url, p.Message); err != nil {
			p.Error = "send failed: " + err.Error()
		} else {
			p.Sent = true
		}
	}
	s.render(w, r, "destination", "preview", page{Data: p})
}
