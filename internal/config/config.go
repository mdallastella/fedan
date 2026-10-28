// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Config holds the process configuration. Feeds, tags and destinations live
// in the database.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// DBPath is the SQLite database file path.
	DBPath string
	// DefaultURL is the default destination's Shoutrrr URL, read on first start only.
	DefaultURL string
	// Interval is the polling interval for every feed.
	Interval time.Duration
	// MaxItemsPerFetch caps the items queued for delivery per feed and fetch.
	MaxItemsPerFetch int
	// FetchConcurrency is the number of feeds fetched in parallel.
	FetchConcurrency int
	// Retention is how long item and delivery history is kept.
	Retention time.Duration
	// PasswordHash is the bcrypt hash of the UI password. Empty means no
	// password, which is only allowed on a loopback address.
	PasswordHash string
	// Password is the plaintext UI password, used when PasswordHash is empty.
	Password string
	// LogFormat is "text" or "json".
	LogFormat string
}

// Auth reports whether the UI requires a password.
func (c Config) Auth() bool {
	return c.PasswordHash != "" || c.Password != ""
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	c := Config{
		DBPath:       env("FEDAN_DB_PATH", "fedan.db"),
		DefaultURL:   os.Getenv("FEDAN_DEFAULT_URL"),
		PasswordHash: os.Getenv("FEDAN_PASSWORD_HASH"),
		Password:     os.Getenv("FEDAN_PASSWORD"),
		LogFormat:    env("FEDAN_LOG_FORMAT", "text"),
	}
	if c.PasswordHash != "" {
		if _, err := bcrypt.Cost([]byte(c.PasswordHash)); err != nil {
			// Typically "$" signs eaten by shell or compose interpolation.
			return c, fmt.Errorf("FEDAN_PASSWORD_HASH is not a bcrypt hash (generate one with fedan hash-password): %w", err)
		}
	}
	// Without a password the UI is only reachable from this machine.
	if c.Auth() {
		c.Addr = env("FEDAN_ADDR", ":8080")
	} else {
		c.Addr = env("FEDAN_ADDR", "127.0.0.1:8080")
		if !loopback(c.Addr) {
			return c, fmt.Errorf("FEDAN_ADDR %s is not a loopback address: set FEDAN_PASSWORD_HASH or FEDAN_PASSWORD", c.Addr)
		}
	}
	var err error
	if c.Retention, err = envDuration("FEDAN_RETENTION", 7*24*time.Hour); err != nil {
		return c, err
	}
	if c.Retention < 24*time.Hour {
		// Deliveries are retried for up to a day; pruning must not cut that short.
		return c, fmt.Errorf("FEDAN_RETENTION must be at least 24h")
	}
	if c.Interval, err = envDuration("FEDAN_INTERVAL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.MaxItemsPerFetch, err = envInt("FEDAN_MAX_ITEMS_PER_FETCH", 10); err != nil {
		return c, err
	}
	if c.FetchConcurrency, err = envInt("FEDAN_FETCH_CONCURRENCY", 4); err != nil {
		return c, err
	}
	if c.Interval < time.Minute {
		return c, fmt.Errorf("FEDAN_INTERVAL must be at least 1m")
	}
	if c.MaxItemsPerFetch < 1 || c.FetchConcurrency < 1 {
		return c, fmt.Errorf("FEDAN_MAX_ITEMS_PER_FETCH and FEDAN_FETCH_CONCURRENCY must be positive")
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return c, fmt.Errorf("FEDAN_LOG_FORMAT must be text or json")
	}
	return c, nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
