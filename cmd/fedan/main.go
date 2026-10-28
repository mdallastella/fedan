// Command fedan pulls RSS/Atom feeds and pushes new items to Shoutrrr destinations.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdallastella/fedan/internal/store"
	"github.com/mdallastella/fedan/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := envOr("FEDAN_ADDR", ":8080")
	dbPath := envOr("FEDAN_DB_PATH", "fedan.db")

	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	defaultURL := os.Getenv("FEDAN_DEFAULT_URL")
	stored, err := store.EnsureDefaultDestination(ctx, db, defaultURL)
	if err != nil {
		return err
	}
	if defaultURL != "" && defaultURL != stored {
		log.Warn("FEDAN_DEFAULT_URL ignored: the default destination is already configured; edit it from the UI")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /", http.FileServerFS(web.Static()))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
