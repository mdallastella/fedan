// Command fedan pulls RSS/Atom feeds and pushes new items to Shoutrrr destinations.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mdallastella/fedan/internal/config"
	"github.com/mdallastella/fedan/internal/fetch"
	"github.com/mdallastella/fedan/internal/store"
	"github.com/mdallastella/fedan/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.LogFormat == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	stored, err := store.EnsureDefaultDestination(ctx, db, cfg.DefaultURL)
	if err != nil {
		return err
	}
	if cfg.DefaultURL != "" && cfg.DefaultURL != stored {
		log.Warn("FEDAN_DEFAULT_URL ignored: the default destination is already configured; edit it from the UI")
	}

	fetcher := fetch.New(db, log, logAlerter{log}, fetch.Options{
		Interval:    cfg.Interval,
		MaxItems:    cfg.MaxItemsPerFetch,
		Concurrency: cfg.FetchConcurrency,
		UserAgent:   "fedan/" + version + " (+https://github.com/mdallastella/fedan)",
	})
	var workers sync.WaitGroup
	workers.Go(func() { fetcher.Run(ctx) })
	defer workers.Wait()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /", http.FileServerFS(web.Static()))

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		stop()
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

// logAlerter logs alerts until the sender can deliver them to the default
// destination.
type logAlerter struct{ log *slog.Logger }

func (a logAlerter) Alert(_ context.Context, title, body string) {
	a.log.Warn("alert", "title", title, "body", body)
}
