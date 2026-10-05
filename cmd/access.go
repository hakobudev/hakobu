package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

// Every user sees only what they own: their projects with the apps,
// databases and storages in them, the servers they added and the
// Cloudflare accounts they connected. checkAccess runs before every
// authed handler and checks whatever the request names, in its path or
// as a chart's target; what isn't the user's is answered as if it didn't
// exist. Handlers that list things filter them on their own.

var errNotFound = errors.New("not found")

func checkAccess(r *http.Request, s *store.Store) error {
	u := requestUser(r)
	ctx := r.Context()
	projectID := func(id int64) error {
		if p, err := s.GetProjectByID(ctx, id); err != nil || p.UserID != u.ID {
			return errNotFound
		}
		return nil
	}
	project := func(name string) error {
		if p, err := s.GetProject(ctx, name); err != nil || p.UserID != u.ID {
			return errNotFound
		}
		return nil
	}
	app := func(name string) error {
		a, err := s.GetApp(ctx, name)
		if err != nil {
			return errNotFound
		}
		return projectID(a.ProjectID)
	}
	checks := []struct {
		param string
		check func(string) error
	}{
		{"p", project},
		{"a", app},
		{"d", func(name string) error {
			d, err := s.GetDatabase(ctx, name)
			if err != nil {
				return errNotFound
			}
			return projectID(d.ProjectID)
		}},
		{"st", func(name string) error {
			st, err := s.GetStorage(ctx, name)
			if err != nil {
				return errNotFound
			}
			return projectID(st.ProjectID)
		}},
		// /sealed/{scope}/{owner}: a project's or an app's (or its
		// worker's) sealed variables.
		{"owner", func(name string) error {
			switch r.PathValue("scope") {
			case "project":
				return project(name)
			case "app", "worker":
				return app(name)
			}
			return errNotFound
		}},
		// /settings/servers/{name} and /settings/clients/{c}.
		{"name", func(name string) error {
			if !strings.HasPrefix(r.URL.Path, "/settings/servers/") {
				return nil
			}
			if n, err := s.GetNodeByName(ctx, name); err != nil || n.UserID != u.ID {
				return errNotFound
			}
			return nil
		}},
		{"c", func(name string) error {
			if a, err := s.GetCloudflareAccountByName(ctx, name); err != nil || a.UserID != u.ID {
				return errNotFound
			}
			return nil
		}},
	}
	for _, c := range checks {
		if v := r.PathValue(c.param); v != "" {
			if err := c.check(v); err != nil {
				return err
			}
		}
	}
	// The usage and uptime charts name what they show as targets.
	if r.URL.Path == "/usage" || r.URL.Path == "/uptime" {
		for _, target := range r.URL.Query()["target"] {
			if err := checkTarget(ctx, s, u, target, app); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTarget lets a user see the charts of their apps and servers (and
// hakobu's services on them), and of what the panel's server shares with
// everyone who deploys on it; a panel-only panel's server is the admin's,
// as is the panel's own uptime.
func checkTarget(ctx context.Context, s *store.Store, u store.User, target string, app func(string) error) error {
	kind, name, _ := strings.Cut(target, ":")
	server := name
	if kind == "service" {
		_, server, _ = strings.Cut(name, ":")
	}
	switch {
	case kind == "app" || kind == "worker":
		return app(name)
	case (kind == ops.HostTarget || kind == "service") && server != "":
		if n, err := s.GetNodeByName(ctx, server); err != nil || n.UserID != u.ID {
			return errNotFound
		}
		return nil
	case (kind == ops.HostTarget || kind == "service") && (!config.PanelOnly || u.Admin == 1):
		return nil
	case target == "panel" && u.Admin == 1:
		return nil
	}
	return errNotFound
}
