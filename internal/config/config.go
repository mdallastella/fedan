// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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
	// LogFormat is "text" or "json".
	LogFormat string
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	c := Config{
		Addr:       env("FEDAN_ADDR", ":8080"),
		DBPath:     env("FEDAN_DB_PATH", "fedan.db"),
		DefaultURL: os.Getenv("FEDAN_DEFAULT_URL"),
		LogFormat:  env("FEDAN_LOG_FORMAT", "text"),
	}
	var err error
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
