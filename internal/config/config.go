// Package config centralizes agent tunables that were previously
// hardcoded across cmd/ and internal/ packages.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Server.
var (
	AgentAddr         = envString("HAKO_ADDR", "127.0.0.1:9000")
	ReadTimeout       = envDuration("HAKO_READ_TIMEOUT", 15*time.Second)
	WriteTimeout      = envDuration("HAKO_WRITE_TIMEOUT", 60*time.Second)
	IdleTimeout       = envDuration("HAKO_IDLE_TIMEOUT", 120*time.Second)
	MaxEnvelopeBytes  = int64(envInt("HAKO_MAX_ENVELOPE_MB", 10)) << 20
	HealthPollEvery   = envDuration("HAKO_HEALTH_POLL", 10*time.Second)
	BackupEvery       = envDuration("HAKO_BACKUP_EVERY", 24*time.Hour)
	RetentionDays     = envInt("HAKO_RETENTION_DAYS", 7)
	WaitHealthyTries  = envInt("HAKO_WAIT_HEALTHY_TRIES", 30)
	MetricsAddr       = envString("HAKO_METRICS_ADDR", "127.0.0.1:9091")
	ProjectBasePort   = envInt("HAKO_BASE_PORT", 8081)
)

// PublicBaseURL is the externally reachable host (no scheme) that deployed
// containers and Sentry SDKs use to reach the agent, e.g. "app.example.com".
// Bring-your-own edge: set HAKO_PUBLIC_HOST or Settings → Access in
// the dashboard. Empty means "not exposed yet" — SENTRY_DSN injection is
// skipped rather than shipping a dead DSN.
func PublicBaseURL() string {
	if v := os.Getenv("HAKO_PUBLIC_HOST"); v != "" {
		return v
	}
	return publicHostFile()
}

// PostgresVersions is the list of supported postgres major versions, newest
// first. Override via HAKO_POSTGRES_VERSIONS="18,17,16".
var PostgresVersions = parsePostgresVersions()

func parsePostgresVersions() []string {
	if v := os.Getenv("HAKO_POSTGRES_VERSIONS"); v != "" {
		var out []string
		for _, p := range splitCSV(v) {
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"18", "17", "16", "15", "14"}
}

func publicHostFile() string {
	b, err := os.ReadFile(filepath.Join("data", "public_host"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetPublicHost persists the dashboard-configured public host (Settings →
// Access). Env var takes precedence when set.
func SetPublicHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return os.Remove(filepath.Join("data", "public_host"))
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join("data", "public_host"), []byte(host+"\n"), 0644)
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
