// Package config is hakobu's own settings that aren't the server's config:
// the images it runs for its databases and caches.
package config

import "os"

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
