package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	AgentAddr        = envString("HAKOBU_ADDR", "127.0.0.1:9000")
	ReadTimeout      = envDuration("HAKOBU_READ_TIMEOUT", 15*time.Second)
	WriteTimeout     = envDuration("HAKOBU_WRITE_TIMEOUT", 5*time.Minute)
	IdleTimeout      = envDuration("HAKOBU_IDLE_TIMEOUT", 120*time.Second)
	ProxyPollEvery   = envDuration("HAKOBU_PROXY_POLL", 10*time.Second)
	HealthCheckEvery = envDuration("HAKOBU_HEALTH_CHECK_EVERY", 30*time.Second)
	BackupEvery      = envDuration("HAKOBU_BACKUP_EVERY", 24*time.Hour)
	BackupKeep       = envInt("HAKOBU_BACKUP_KEEP", 7) // plus one a week for four weeks
	RetentionDays    = envInt("HAKOBU_RETENTION_DAYS", 7)
	// PanelOnly is a panel shared by people who don't trust each other's
	// apps, such as a hosted one: its own server runs none, and its
	// Cloudflare account serves only the panel. Each project runs on a
	// server of its owner's, in a Cloudflare account of theirs.
	PanelOnly = envBool("HAKOBU_PANEL_ONLY")
	// SharedDomain is a domain of the panel's Cloudflare account whose
	// names the panel gives out: on a panel-only panel, users the admin
	// marks may make projects in the panel's account, and their apps take
	// addresses <name>.<SharedDomain> (one level, so Cloudflare's free
	// certificate covers them). Empty: none given out.
	SharedDomain = strings.ToLower(strings.Trim(os.Getenv("HAKOBU_SHARED_DOMAIN"), ". "))
)

// The master key lives outside data/, so a copy of data/ (a backup of the
// database, say) doesn't carry the key to its secrets.
const (
	DatabaseFile  = "data/hakobu.db"
	MasterKeyFile = "key/master.key"
	// KeyExportedFile holds the fingerprint of the master key the owner
	// last downloaded or exported, so a server can tell they have a copy.
	KeyExportedFile = "data/key_downloaded"
)

const (
	publicHostFile = "data/public_host"
	appsDomainFile = "data/apps_domain"
	setupTokenFile = "data/setup_token"
)

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func envBool(key string) bool {
	b, _ := strconv.ParseBool(os.Getenv(key))
	return b
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}
