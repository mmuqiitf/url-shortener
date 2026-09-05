// Package config loads runtime configuration from environment variables.
//
// All settings have sensible defaults; an empty or unset variable means
// "use the default". The function returns an error only when a provided
// value is syntactically invalid (for example a non-integer port).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the server.
//
// Field tags document the env var name and the default. The struct is
// constructed by Load() and passed to constructors via dependency
// injection — no package reaches for os.Getenv on its own.
type Config struct {
	Port     string     // env: PORT,              default: 8080
	BaseURL  string     // env: BASE_URL,          default: http://localhost:8080
	LogLevel slog.Level // env: LOG_LEVEL,         default: info

	DBPath         string // env: DB_PATH,            default: ./data/shortener.db
	DBMaxOpenConns int    // env: DB_MAX_OPEN_CONNS,  default: 1 (SQLite-friendly)
	DBMaxIdleConns int    // env: DB_MAX_IDLE_CONNS,  default: 1

	TrackerWorkers int           // env: TRACKER_WORKERS,          default: 2
	TrackerBuffer  int           // env: TRACKER_BUFFER,           default: 4096
	TrackerBatch   int           // env: TRACKER_BATCH_SIZE,       default: 50
	TrackerFlush   time.Duration // env: TRACKER_FLUSH_INTERVAL_MS default: 1000ms

	RedisAddr     string        // env: REDIS_ADDR,               default: localhost:6379
	RedisPassword string        // env: REDIS_PASSWORD,           default: ""
	RedisDB       int           // env: REDIS_DB,                 default: 0
	RedisTTL      time.Duration // env: REDIS_TTL_SEC,            default: 600s

	shutdownTimeout time.Duration // env: SHUTDOWN_TIMEOUT_MS, default: 15000ms
}

// Load reads configuration from the environment and applies defaults.
//
// It is safe to call Load() once during process start. The returned Config
// is treated as immutable by the rest of the application.
func Load() (*Config, error) {
	cfg := &Config{
		Port:            getenv("PORT", "8080"),
		BaseURL:         getenv("BASE_URL", ""),
		DBPath:          getenv("DB_PATH", "./data/shortener.db"),
		DBMaxOpenConns:  getenvInt("DB_MAX_OPEN_CONNS", 1),
		DBMaxIdleConns:  getenvInt("DB_MAX_IDLE_CONNS", 1),
		TrackerWorkers:  getenvInt("TRACKER_WORKERS", 2),
		TrackerBuffer:   getenvInt("TRACKER_BUFFER", 4096),
		TrackerBatch:    getenvInt("TRACKER_BATCH_SIZE", 50),
		TrackerFlush:    time.Duration(getenvInt("TRACKER_FLUSH_INTERVAL_MS", 1000)) * time.Millisecond,
		RedisAddr:       getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:   getenv("REDIS_PASSWORD", ""),
		RedisDB:         getenvInt("REDIS_DB", 0),
		RedisTTL:        time.Duration(getenvInt("REDIS_TTL_SEC", 600)) * time.Second,
		shutdownTimeout: time.Duration(getenvInt("SHUTDOWN_TIMEOUT_MS", 15000)) * time.Millisecond,
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:" + cfg.Port
	}

	level, err := parseLogLevel(getenv("LOG_LEVEL", "info"))
	if err != nil {
		return nil, fmt.Errorf("config: LOG_LEVEL: %w", err)
	}
	cfg.LogLevel = level

	if cfg.TrackerWorkers < 1 {
		return nil, errors.New("config: TRACKER_WORKERS must be >= 1")
	}
	if cfg.TrackerBuffer < 1 {
		return nil, errors.New("config: TRACKER_BUFFER must be >= 1")
	}
	if cfg.TrackerBatch < 1 {
		return nil, errors.New("config: TRACKER_BATCH_SIZE must be >= 1")
	}
	if cfg.DBMaxOpenConns < 1 {
		return nil, errors.New("config: DB_MAX_OPEN_CONNS must be >= 1")
	}

	return cfg, nil
}

// ShutdownTimeout returns the configured graceful-shutdown deadline.
func (c *Config) ShutdownTimeout() time.Duration { return c.shutdownTimeout }

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (use debug|info|warn|error)", s)
	}
}
