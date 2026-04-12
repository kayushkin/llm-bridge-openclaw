package main

import "os"

// Config holds environment-based configuration for the harness.
type Config struct {
	OpenClawURL string
	OpenClawDir string
	Token       string
}

func loadConfig() *Config {
	return &Config{
		OpenClawURL: envOr("OPENCLAW_URL", "http://127.0.0.1:18789"),
		OpenClawDir: os.Getenv("OPENCLAW_DIR"),
		Token:       os.Getenv("OPENCLAW_TOKEN"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
