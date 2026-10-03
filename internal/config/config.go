// Package config loads the server configuration from environment variables.
//
// For local development, values can also come from a .env file in the current
// folder. Real environment variables always win over values in .env.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// MaxServerNameLength limits the server name shown to clients.
const MaxServerNameLength = 64

// Config holds every setting the server needs.
type Config struct {
	// ServerName is shown to clients in GET /api/v1/info.
	ServerName string
	// ListenAddr is the address the HTTP server listens on, e.g. "127.0.0.1:8080".
	ListenAddr string
}

// Load reads the configuration. A missing .env file is fine; an unreadable one is an error.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("reading .env: %w", err)
	}

	cfg := Config{
		ServerName: getEnv("VIANDEN_SERVER_NAME", "Vianden Server"),
		ListenAddr: getEnv("VIANDEN_LISTEN_ADDR", "127.0.0.1:8080"),
	}

	if len(cfg.ServerName) > MaxServerNameLength {
		return Config{}, fmt.Errorf("VIANDEN_SERVER_NAME is longer than %d characters", MaxServerNameLength)
	}
	return cfg, nil
}

// getEnv returns the trimmed value of an environment variable, or fallback if it is unset or empty.
func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
