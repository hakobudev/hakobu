package ops

import "github.com/x0ryz/hakobu/internal/store"

// PublicURL is where the app is reachable from the internet, "" if nowhere.
func PublicURL(app store.App) string {
	if app.Domain == "" {
		return ""
	}
	return "https://" + app.Domain
}
