package ui

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/mdallastella/fedan/internal/store"
)

type deliveryRow struct {
	ID              int64
	Status          string
	Attempts        int
	LastError       string
	NextAttemptAt   string
	SentAt          sql.NullString
	CreatedAt       string
	ItemTitle, Link string
	FeedID          int64
	FeedTitle       string
	DestinationID   int64
	Destination     string
}

// listDeliveries returns deliveries, newest first; limit 0 means no limit.
func (s *Server) listDeliveries(ctx context.Context, where string, limit int, args ...any) ([]deliveryRow, error) {
	q := `SELECT dl.id, dl.status, dl.attempts, dl.last_error, dl.next_attempt_at, dl.sent_at, dl.created_at,
			i.title, i.link, f.id, coalesce(nullif(f.title, ''), f.url), ds.id, ds.name
		FROM deliveries dl
		JOIN items i ON i.id = dl.item_id
		JOIN feeds f ON f.id = i.feed_id
		JOIN destinations ds ON ds.id = dl.destination_id ` + where + ` ORDER BY dl.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.ID, &d.Status, &d.Attempts, &d.LastError, &d.NextAttemptAt, &d.SentAt, &d.CreatedAt,
			&d.ItemTitle, &d.Link, &d.FeedID, &d.FeedTitle, &d.DestinationID, &d.Destination); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type deliveriesData struct {
	Failed, Pending []deliveryRow
}

func (s *Server) deliveries(w http.ResponseWriter, r *http.Request) {
	var d deliveriesData
	var err error
	if d.Failed, err = s.listDeliveries(r.Context(), "WHERE dl.status = 'failed'", 200); err != nil {
		s.fail(w, err)
		return
	}
	if d.Pending, err = s.listDeliveries(r.Context(), "WHERE dl.status = 'pending'", 200); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "deliveries", "", page{Title: "Deliveries", Nav: "deliveries", Data: d})
}

func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := store.RetryDelivery(r.Context(), s.db, id); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/deliveries", "Delivery queued again.")
}

func (s *Server) retryFailed(w http.ResponseWriter, r *http.Request) {
	now := store.Now()
	res, err := s.db.ExecContext(r.Context(), `UPDATE deliveries SET status = 'pending', attempts = 0, last_error = '',
		next_attempt_at = ?, created_at = ? WHERE status = 'failed'`, now, now)
	if err != nil {
		s.fail(w, err)
		return
	}
	n, _ := res.RowsAffected()
	redirect(w, r, "/deliveries", fmt.Sprintf("%d deliveries queued again.", n))
}
