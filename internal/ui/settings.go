package ui

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/mdallastella/fedan/internal/opml"
	"github.com/mdallastella/fedan/internal/store"
)

type settingRow struct{ Name, Env, Value string }

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	c := s.cfg
	auth := "off (loopback only)"
	if c.Auth() {
		auth = "on"
	}
	rows := []settingRow{
		{"Listen address", "FEDAN_ADDR", c.Addr},
		{"Database", "FEDAN_DB_PATH", c.DBPath},
		{"Polling interval", "FEDAN_INTERVAL", c.Interval.String()},
		{"Max items per fetch", "FEDAN_MAX_ITEMS_PER_FETCH", fmt.Sprint(c.MaxItemsPerFetch)},
		{"Parallel fetches", "FEDAN_FETCH_CONCURRENCY", fmt.Sprint(c.FetchConcurrency)},
		{"History retention", "FEDAN_RETENTION", c.Retention.String()},
		{"Password", "FEDAN_PASSWORD_HASH / FEDAN_PASSWORD", auth},
		{"Log format", "FEDAN_LOG_FORMAT", c.LogFormat},
	}
	s.render(w, r, "settings", "", page{Title: "Settings", Nav: "settings", Data: rows})
}

func (s *Server) exportOPML(w http.ResponseWriter, r *http.Request) {
	feeds, err := s.listFeeds(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]opml.Feed, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, opml.Feed{URL: f.URL, Title: f.Name(), SiteURL: f.SiteURL, Tags: f.Tags})
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fedan.opml"`)
	if err := opml.Write(w, "fēdan feeds", out); err != nil {
		s.log.Error("export opml", "err", err)
	}
}

// importOPML adds every feed of an uploaded OPML file, tagged main plus its
// folders and categories. Feeds already present are skipped. Imported feeds
// are fetched by the scheduler; their first fetch sends nothing.
func (s *Server) importOPML(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		redirect(w, r, "/settings", "Choose an OPML file to import.")
		return
	}
	defer func() { _ = file.Close() }()
	feeds, err := opml.Parse(file)
	if err != nil {
		redirect(w, r, "/settings", err.Error())
		return
	}
	var added, skipped int
	for _, f := range feeds {
		tags := []string{store.MainTag}
		for _, t := range f.Tags {
			if t = store.Slug(t); t != "" && !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
		_, err := store.AddFeed(r.Context(), s.db, f.URL, f.Title, tags)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			skipped++
		case err != nil:
			s.fail(w, err)
			return
		default:
			added++
		}
	}
	redirect(w, r, "/feeds", fmt.Sprintf("Imported %d feeds (%d already present).", added, skipped))
}
