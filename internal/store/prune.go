package store

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// Prune deletes items (with their deliveries) older than retention, and seen
// keys absent from the feed for longer than retention. A key counts as absent
// only once a successful fetch no longer contained it, so a feed that stops
// answering keeps its keys and can't resend old entries when it comes back.
func Prune(ctx context.Context, db *sql.DB, now time.Time, retention time.Duration) (items, keys int64, err error) {
	cutoff := Time(now.Add(-retention))
	res, err := db.ExecContext(ctx, "DELETE FROM items WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, 0, err
	}
	items, _ = res.RowsAffected()
	res, err = db.ExecContext(ctx, `DELETE FROM seen_keys WHERE last_seen_at < ?
		AND last_seen_at < (SELECT last_success_at FROM feeds WHERE feeds.id = seen_keys.feed_id)`, cutoff)
	if err != nil {
		return items, 0, err
	}
	keys, _ = res.RowsAffected()
	return items, keys, nil
}

// RunPrune prunes hourly until ctx is cancelled.
func RunPrune(ctx context.Context, db *sql.DB, log *slog.Logger, retention time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		items, keys, err := Prune(ctx, db, time.Now(), retention)
		switch {
		case err != nil:
			log.Error("prune", "err", err)
		case items > 0 || keys > 0:
			log.Info("pruned", "items", items, "seen_keys", keys)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
