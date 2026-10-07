package cmd

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

// Every user sees only what they own: their projects with the apps,
// databases and storages in them, the servers they added and the
// Cloudflare accounts they connected. Names are theirs alone: a project's
// among the user's projects, an app's, database's or storage's in its
// project. resolve looks up what the request's path names that way, before
// every authed handler, so what isn't the user's is never found; handlers
// take it from the request (reqProject, reqApp, ...). Handlers that list
// things filter them on their own.

var errNotFound = errors.New("not found")

// resolved is what the request's path names.
type resolved struct {
	project *store.Project
	app     *store.App
	db      *store.Database
	storage *store.Storage
	cache   *store.Cache
}

type resolvedKey struct{}

func resolve(r *http.Request, s *store.Store) (*http.Request, error) {
	u := requestUser(r)
	ctx := r.Context()
	var res resolved
	if name := r.PathValue("p"); name != "" {
		p, err := s.GetProject(ctx, store.GetProjectParams{UserID: u.ID, Name: name})
		if err != nil {
			return r, errNotFound
		}
		res.project = &p
		if name := r.PathValue("a"); name != "" {
			a, err := s.GetApp(ctx, store.GetAppParams{ProjectID: p.ID, Name: name})
			if err != nil {
				return r, errNotFound
			}
			res.app = &a
		}
		if name := r.PathValue("d"); name != "" {
			d, err := s.GetDatabase(ctx, store.GetDatabaseParams{ProjectID: p.ID, Name: name})
			if err != nil {
				return r, errNotFound
			}
			res.db = &d
		}
		if name := r.PathValue("st"); name != "" {
			st, err := s.GetStorage(ctx, store.GetStorageParams{ProjectID: p.ID, Name: name})
			if err != nil {
				return r, errNotFound
			}
			res.storage = &st
		}
		if name := r.PathValue("cache"); name != "" {
			c, err := s.GetCache(ctx, store.GetCacheParams{ProjectID: p.ID, Name: name})
			if err != nil {
				return r, errNotFound
			}
			res.cache = &c
		}
	}
	// /settings/servers/{name} and /settings/clients/{c}.
	if name := r.PathValue("name"); name != "" && strings.HasPrefix(r.URL.Path, "/settings/servers/") {
		if _, err := ops.UserServer(s, u.ID, name); err != nil {
			return r, errNotFound
		}
	}
	if name := r.PathValue("c"); name != "" {
		if a, err := s.GetCloudflareAccountByName(ctx, name); err != nil || a.UserID != u.ID {
			return r, errNotFound
		}
	}
	// The usage and uptime charts name what they show as targets.
	if r.URL.Path == "/usage" || r.URL.Path == "/uptime" {
		for _, target := range r.URL.Query()["target"] {
			if err := checkTarget(ctx, s, u, target); err != nil {
				return r, err
			}
		}
	}
	return r.WithContext(context.WithValue(ctx, resolvedKey{}, res)), nil
}

func requestResolved(r *http.Request) resolved {
	res, _ := r.Context().Value(resolvedKey{}).(resolved)
	return res
}

// reqProject is the project the request's path names; a route with {p}
// in it always has one.
func reqProject(r *http.Request) store.Project { return *requestResolved(r).project }

// reqApp is the app the request's path names ({p} and {a}).
func reqApp(r *http.Request) store.App { return *requestResolved(r).app }

// reqDB is the database the request's path names ({p} and {d}).
func reqDB(r *http.Request) store.Database { return *requestResolved(r).db }

// reqStorage is the storage the request's path names ({p} and {st}).
func reqStorage(r *http.Request) store.Storage { return *requestResolved(r).storage }

// reqCache is the cache the request's path names ({p} and {cache}).
func reqCache(r *http.Request) store.Cache { return *requestResolved(r).cache }

// checkTarget lets a user see the charts of their apps and servers (and
// hakobu's services on them), and of what the panel's server shares with
// everyone who deploys on it; a panel-only panel's server is the admin's,
// as is the panel's own uptime.
func checkTarget(ctx context.Context, s *store.Store, u store.User, target string) error {
	kind, rest, _ := strings.Cut(target, ":")
	server := rest
	if kind == "service" {
		_, server, _ = strings.Cut(rest, ":")
	}
	switch {
	case kind == "app" || kind == "worker":
		id, _ := strconv.ParseInt(rest, 10, 64)
		if a, err := s.GetAppByID(ctx, id); err != nil || a.UserID != u.ID {
			return errNotFound
		}
		return nil
	case (kind == ops.HostTarget || kind == "service") && server != "":
		id, _ := strconv.ParseInt(server, 10, 64)
		if n, err := s.GetNode(ctx, id); err != nil || n.UserID != u.ID {
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
