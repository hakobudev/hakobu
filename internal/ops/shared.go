package ops

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/store"
)

// Shared addresses. A panel-only panel with HAKOBU_SHARED_DOMAIN gives the
// users its admin marks projects in its own Cloudflare account, on servers
// of theirs. Their apps take addresses <name>.<shared domain> and nothing
// else of the account: one level, so Cloudflare's free certificate covers
// them, and first come first served.

// SharedDomainOn: the panel gives out addresses in its shared domain.
func SharedDomainOn() bool { return config.PanelOnly && config.SharedDomain != "" }

// CanShareDomain: user may have projects in the panel's account.
func CanShareDomain(s *store.Store, userID int64) bool {
	if !SharedDomainOn() {
		return false
	}
	u, err := s.GetUser(ctx(), userID)
	return err == nil && u.SharedDomain == 1
}

// inSharedDomain: the project's apps take addresses in the shared domain.
func inSharedDomain(p store.Project) bool {
	return config.PanelOnly && !p.CloudflareAccountID.Valid
}

// errNotShared is what a panel-only panel says to a user it doesn't give
// addresses to, about a project in its account.
var errNotShared = errors.New("this panel runs no apps of its own: a project runs on a server of yours (Settings → Servers), in a Cloudflare account of yours (Settings → Cloudflare); its admin may give you addresses of its own")

var (
	validSharedName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])$`)
	// Names the panel's own services take, or may, or that look official.
	reservedSharedNames = []string{
		"www", "dash", "docs", "api", "admin", "mail", "status", "login", "auth", "cdn", "hakobu",
		"app", "apps", "panel", "mcp", "oauth", "sso", "signin", "signup", "register", "account", "accounts",
		"billing", "pay", "support", "help", "blog", "news", "about", "security", "abuse", "postmaster",
		"hostmaster", "webmaster", "root", "smtp", "imap", "pop", "webmail", "mx", "ns1", "ns2", "dns",
		"autoconfig", "autodiscover", "ftp", "static", "assets", "media", "files", "download", "downloads",
		"releases", "updates", "install", "webhook", "webhooks", "health", "metrics", "internal",
		"staging", "dev", "test", "demo", "beta", "github", "cloudflare",
	}
)

// checkSharedAddress checks domain as an address of an app of project p in
// the shared domain, if p takes those; "" (private) always fits.
func checkSharedAddress(s *store.Store, p store.Project, domain string) error {
	if domain == "" || !inSharedDomain(p) {
		return nil
	}
	if !CanShareDomain(s, p.UserID) {
		return errNotShared
	}
	name, ok := strings.CutSuffix(domain, "."+config.SharedDomain)
	if !ok || strings.Contains(name, ".") {
		return fmt.Errorf("%s isn't yours to take: apps here take addresses <name>.%s", domain, config.SharedDomain)
	}
	return checkSharedName(name)
}

func checkSharedName(name string) error {
	switch {
	case len(name) < 3:
		return fmt.Errorf("%q is too short: an address takes 3 characters or more", name)
	case !validSharedName.MatchString(name):
		return fmt.Errorf("%q can't be an address: use lowercase letters, digits and dashes, not first or last", name)
	case len(name) >= 4 && name[2:4] == "--":
		// xn-- and the other prefixes DNS keeps for encoded names
		return fmt.Errorf("%q can't be an address: no two dashes after its first two characters", name)
	case slices.Contains(reservedSharedNames, name):
		return fmt.Errorf("%s.%s is kept for the panel: pick another name", name, config.SharedDomain)
	}
	return nil
}

// SetUserSharedDomain lets a user have projects in the panel's account,
// with addresses in its shared domain, or no longer: projects they have
// keep their addresses, but no new ones are taken.
func SetUserSharedDomain(s *store.Store, id int64, shared bool) error {
	if _, err := s.GetUser(ctx(), id); err != nil {
		return err
	}
	flag := int64(0)
	if shared {
		flag = 1
	}
	return s.SetUserSharedDomain(ctx(), store.SetUserSharedDomainParams{SharedDomain: flag, ID: id})
}
