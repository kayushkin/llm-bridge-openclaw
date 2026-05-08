package main

import (
	"os"
	"path/filepath"
)

// Config holds environment-based configuration for the harness.
type Config struct {
	OpenClawURL string
	OpenClawDir string
	Token       string
}

func loadConfig() *Config {
	return &Config{
		OpenClawURL: envOr("OPENCLAW_URL", "http://127.0.0.1:18789"),
		OpenClawDir: envOr("OPENCLAW_DIR", defaultOpenClawDir()),
		Token:       os.Getenv("OPENCLAW_TOKEN"),
	}
}

// defaultOpenClawDir returns ~/.openclaw, the conventional location of the
// OpenClaw on-disk session store. Returns "" when the home directory cannot
// be resolved; callers should treat an empty OpenClawDir as "no local store".
func defaultOpenClawDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".openclaw")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
