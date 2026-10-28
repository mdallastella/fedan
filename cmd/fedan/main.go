// Command fedan pulls RSS/Atom feeds and pushes new items to Shoutrrr destinations.
//
//	fedan [serve]         run the daemon and web UI (default)
//	fedan hash-password   read a password from stdin, print its bcrypt hash for FEDAN_PASSWORD_HASH
//	fedan healthcheck     exit non-zero unless the local server answers /healthz
//	fedan version         print the version
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mdallastella/fedan/internal/config"
	"github.com/mdallastella/fedan/internal/fetch"
	"github.com/mdallastella/fedan/internal/send"
	"github.com/mdallastella/fedan/internal/store"
	"github.com/mdallastella/fedan/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(os.Args[1:], log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(args []string, log *slog.Logger) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
	case "hash-password":
		return hashPassword()
	case "version":
		fmt.Println(version)
		return nil
	case "healthcheck":
	case "help", "-h", "--help":
		fmt.Println("usage: fedan [serve | hash-password | healthcheck | version]")
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cmd == "healthcheck" {
		return healthcheck(cfg.Addr)
	}
	if cfg.LogFormat == "json" {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return serve(cfg, log)
}

func serve(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if cfg.DefaultURL != "" {
		if err := send.Validate(cfg.DefaultURL); err != nil {
			return fmt.Errorf("FEDAN_DEFAULT_URL: %w", err)
		}
	}
	stored, err := store.EnsureDefaultDestination(ctx, db, cfg.DefaultURL)
	if err != nil {
		return err
	}
	if cfg.DefaultURL != "" && cfg.DefaultURL != stored {
		log.Warn("FEDAN_DEFAULT_URL ignored: the default destination is already configured; edit it from the UI")
	}

	sender := send.New(db, log, send.Shoutrrr)
	fetcher := fetch.New(db, log, sender, fetch.Options{
		Interval:    cfg.Interval,
		MaxItems:    cfg.MaxItemsPerFetch,
		Concurrency: cfg.FetchConcurrency,
		UserAgent:   "fedan/" + version + " (+https://github.com/mdallastella/fedan)",
	})
	var workers sync.WaitGroup
	workers.Go(func() { fetcher.Run(ctx) })
	workers.Go(func() { sender.Run(ctx) })
	workers.Go(func() { store.RunPrune(ctx, db, log, cfg.Retention) })
	defer workers.Wait()

	sessionKey, err := store.Secret(ctx, db, "session_key", 32)
	if err != nil {
		return err
	}
	web, err := ui.New(db, log, cfg, fetcher, sender, version, []byte(sessionKey))
	if err != nil {
		return err
	}
	if !cfg.Auth() {
		log.Warn("no FEDAN_PASSWORD_HASH or FEDAN_PASSWORD set: the UI is open, listening on loopback only")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", web.Handler())

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

func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no password read from stdin")
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return errors.New("empty password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse FEDAN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
