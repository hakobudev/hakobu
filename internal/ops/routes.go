package ops

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// Routes put another app of the project behind a path of an app's
// address, like a reverse proxy in front of both: with /api → api,
// shop.example.com/api/... reaches the api app, everything else the
// shop. One address, so the browser needs no CORS and shares cookies. The
// tunnel routes them by itself; the api sees the whole path, /api
// included.

var validRoutePath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// SetAppRoute sends the app's address's path (a prefix such as /api) to
// target, another app of its project by its name; target "" removes the
// route.
func SetAppRoute(s *store.Store, app store.App, routePath, target string) error {
	var err error
	routePath = strings.TrimSpace(routePath)
	if routePath != "" && routePath != "/" {
		routePath = path.Clean("/" + strings.TrimPrefix(routePath, "/"))
	}
	if !validRoutePath.MatchString(routePath) {
		return fmt.Errorf("a route's path is like /api: letters, digits, dots, dashes and underscores between slashes, not / alone")
	}
	if target == "" {
		err = s.DeleteAppRoute(ctx(), store.DeleteAppRouteParams{AppID: app.ID, Path: routePath})
	} else {
		t, terr := s.GetApp(ctx(), store.GetAppParams{ProjectID: app.ProjectID, Name: target})
		switch {
		case terr != nil:
			return fmt.Errorf("%s isn't an app of project %s", target, app.ProjectName)
		case t.ID == app.ID:
			return fmt.Errorf("a route goes to another app")
		}
		err = s.SetAppRoute(ctx(), store.SetAppRouteParams{AppID: app.ID, Path: routePath, TargetID: t.ID})
	}
	if err != nil {
		return err
	}
	async(func() {
		if err := SyncTunnel(s); err != nil {
			panellog.Warn("tunnel routes not updated:", err)
		}
	})
	return nil
}

// routeRules are the ingress rules for app's routes, longest path first,
// to go before the rule for its address; a target not deployed yet has
// none.
func routeRules(app store.App, routes []store.AppRoute, live map[int64]int64) []cloudflare.IngressRule {
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].Path) > len(routes[j].Path) })
	var rules []cloudflare.IngressRule
	for _, r := range routes {
		if port := live[r.TargetID]; port > 0 {
			rules = append(rules, cloudflare.IngressRule{
				Hostname: app.Domain, Path: "^" + regexp.QuoteMeta(r.Path) + "(/|$)",
				Service: fmt.Sprintf("http://%s:%d", EdgeAlias(store.AppKey(r.TargetID)), port),
			})
		}
	}
	return rules
}
