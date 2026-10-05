package ops

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

// Routes put another app of the project behind a path of an app's
// address, like a reverse proxy in front of both: with /api → api,
// shop.example.com/api/... reaches the api app, everything else the
// shop. One address, so the browser needs no CORS and shares cookies. The
// tunnel routes them by itself; the api sees the whole path, /api
// included.

var validRoutePath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// SetAppRoute sends the app's address's path (a prefix such as /api) to
// target, another app of its project; target "" removes the route.
func SetAppRoute(s *store.Store, appName, routePath, target string) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return fmt.Errorf("app %q not found", appName)
	}
	routePath = strings.TrimSpace(routePath)
	if routePath != "" && routePath != "/" {
		routePath = path.Clean("/" + strings.TrimPrefix(routePath, "/"))
	}
	if !validRoutePath.MatchString(routePath) {
		return fmt.Errorf("a route's path is like /api: letters, digits, dots, dashes and underscores between slashes, not / alone")
	}
	if target == "" {
		err = s.DeleteAppRoute(ctx(), store.DeleteAppRouteParams{AppName: appName, Path: routePath})
	} else {
		t, terr := s.GetApp(ctx(), target)
		switch {
		case terr != nil || t.ProjectID != app.ProjectID:
			return fmt.Errorf("%s isn't an app of project %s", target, app.ProjectName)
		case t.Name == app.Name:
			return fmt.Errorf("a route goes to another app")
		}
		err = s.SetAppRoute(ctx(), store.SetAppRouteParams{AppName: appName, Path: routePath, Target: target})
	}
	if err != nil {
		return err
	}
	async(func() {
		if err := SyncTunnel(s); err != nil {
			fmt.Println("tunnel routes not updated:", err)
		}
	})
	return nil
}

// routeRules are the ingress rules for app's routes, longest path first,
// to go before the rule for its address; a target not deployed yet has
// none.
func routeRules(app store.App, routes []store.AppRoute, live map[string]int64) []cloudflare.IngressRule {
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].Path) > len(routes[j].Path) })
	var rules []cloudflare.IngressRule
	for _, r := range routes {
		if port := live[r.Target]; port > 0 {
			rules = append(rules, cloudflare.IngressRule{
				Hostname: app.Domain, Path: "^" + regexp.QuoteMeta(r.Path) + "(/|$)",
				Service: fmt.Sprintf("http://%s:%d", EdgeAlias(r.Target), port),
			})
		}
	}
	return rules
}
