package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/ops"
	"github.com/hakobudev/hakobu/internal/store"
)

// Setting up: tools an AI app uses to deploy a repo from nothing, as the
// user would in the panel: pick where it runs, make the project, the app,
// its database and volumes, and set its variables. They need the deploy
// scope. Nothing here deletes, and secret values stay out: the AI app
// asks the user to seal them in the panel.

type mcpPlaces struct {
	Projects       []mcpProject `json:"projects"`
	Servers        []string     `json:"servers" jsonschema:"the user's servers a new project can run on"`
	Accounts       []string     `json:"cloudflare_accounts" jsonschema:"the user's Cloudflare accounts a new project can use"`
	PanelOnly      bool         `json:"panel_only,omitempty" jsonschema:"a new project must name one of servers and of cloudflare_accounts: the panel runs no apps itself"`
	SharedDomain   string       `json:"shared_domain,omitempty" jsonschema:"on a panel_only panel, a new project with an empty cloudflare_account goes in the panel's account anyway, and its apps take addresses <name>.shared_domain (one level, 3+ characters, first come first served)"`
	SettingsURL    string       `json:"settings_url" jsonschema:"where the user adds a server or a Cloudflare account"`
	RepoAccessHint string       `json:"repo_access,omitempty" jsonschema:"where the user gives the GitHub App access to more repositories"`
}

type mcpProject struct {
	Name      string   `json:"name"`
	Server    string   `json:"server" jsonschema:"empty for the panel's own"`
	Account   string   `json:"cloudflare_account" jsonschema:"empty for the panel's own"`
	Domains   []string `json:"domains" jsonschema:"zones an app's address can be in"`
	Apps      []string `json:"apps"`
	Databases []string `json:"databases"`
	Caches    []string `json:"caches"`
	Storages  []string `json:"storages" jsonschema:"buckets for files; an app gets one's keys when it's linked in the panel"`
}

type mcpPreset struct {
	Path     string `json:"build_path" jsonschema:". for the repo's root"`
	Strategy string `json:"build_strategy" jsonschema:"railpack or dockerfile"`
	Stack    string `json:"stack,omitempty"`
	Port     int    `json:"port,omitempty" jsonschema:"from the Dockerfile's EXPOSE"`
}

type mcpDone struct {
	Done string `json:"done"`
	Next string `json:"next,omitempty"`
}

func addSetupTools(server *mcp.Server, s *store.Store, getApp func(context.Context, string) (store.App, error)) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}
	creates := &mcp.ToolAnnotations{DestructiveHint: new(false), OpenWorldHint: new(false)}
	mayDeploy := func(req *mcp.CallToolRequest) error {
		if ti := req.Extra.TokenInfo; ti == nil || !slices.Contains(ti.Scopes, scopeDeploy) {
			return fmt.Errorf("this connection may only read: reconnect it and allow deploys")
		}
		return nil
	}

	mcp.AddTool(server, &mcp.Tool{Name: "list_projects", Description: "Where apps can go: the user's projects (with their server, Cloudflare account, the domains an app's address can be in, apps and databases), and the servers and Cloudflare accounts a new project can use. Start here before setting up a new app.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcpPlaces, error) {
			u := mcpUserOf(ctx)
			where := placesOf(s, u)
			out := mcpPlaces{Projects: []mcpProject{}, Servers: where.Servers, Accounts: where.Clients, PanelOnly: where.PanelOnly, SharedDomain: where.Shared, SettingsURL: "https://" + config.PublicHost() + "/settings"}
			if out.Servers == nil {
				out.Servers = []string{}
			}
			if out.Accounts == nil {
				out.Accounts = []string{}
			}
			projects, err := s.ListProjectsOf(ctx, u.ID)
			if err != nil {
				return nil, out, err
			}
			for _, p := range projects {
				mp := mcpProject{Name: p.Name, Server: ops.ProjectServerName(s, p), Domains: []string{}, Apps: []string{}, Databases: []string{}, Caches: []string{}, Storages: []string{}}
				if p.CloudflareAccountID.Valid {
					if a, err := s.GetCloudflareAccount(ctx, p.CloudflareAccountID.Int64); err == nil {
						mp.Account = a.Name
					}
				}
				if zones, _, err := ops.ProjectZones(s, p); err == nil {
					mp.Domains = zones
				}
				if apps, err := s.ListAppsByProject(ctx, p.ID); err == nil {
					for _, a := range apps {
						mp.Apps = append(mp.Apps, p.Name+"/"+a.Name)
					}
				}
				if dbs, err := s.ListDatabasesByProject(ctx, p.ID); err == nil {
					for _, d := range dbs {
						mp.Databases = append(mp.Databases, d.Name)
					}
				}
				if caches, err := s.ListCachesByProject(ctx, p.ID); err == nil {
					for _, c := range caches {
						mp.Caches = append(mp.Caches, c.Name)
					}
				}
				if storages, err := s.ListStoragesByProject(ctx, p.ID); err == nil {
					for _, st := range storages {
						mp.Storages = append(mp.Storages, st.Name)
					}
				}
				out.Projects = append(out.Projects, mp)
			}
			if app, err := s.GetGitHubApp(ctx); err == nil {
				out.RepoAccessHint = "https://github.com/apps/" + app.Slug + "/installations/new"
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "list_repos", Description: "List the GitHub repositories the user can deploy (owner/name): those the GitHub App may read on their account. A missing one needs access given in GitHub (list_projects' repo_access).", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct {
			Repos []string `json:"repos"`
		}, error) {
			var out struct {
				Repos []string `json:"repos"`
			}
			repos, err := ops.ListRepos(s, mcpUserOf(ctx).GitHubID)
			if err != nil {
				return nil, out, err
			}
			out.Repos = append([]string{}, repos...)
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "scan_repo", Description: "Find what in a repository can be built, without cloning it: each directory with a Dockerfile or a stack Railpack builds (Node, Python, Go...), the way to build it, and the port a Dockerfile exposes.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Repo string `json:"repo" jsonschema:"owner/name, from list_repos"`
		}) (*mcp.CallToolResult, struct {
			Presets []mcpPreset `json:"buildable"`
		}, error) {
			var out struct {
				Presets []mcpPreset `json:"buildable"`
			}
			presets, err := ops.ScanRepoPresets(s, mcpUserOf(ctx).GitHubID, strings.TrimSpace(in.Repo))
			if err != nil {
				return nil, out, err
			}
			out.Presets = []mcpPreset{}
			for _, p := range presets {
				out.Presets = append(out.Presets, mcpPreset{Path: p.Path, Strategy: p.Strategy, Stack: p.Stack, Port: p.Port})
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "create_project", Description: "Create a project: a group of apps with the databases they use, on one server and in one Cloudflare account (their names from list_projects; empty for the panel's own, unless panel_only: then the account may be empty only when list_projects gives a shared_domain).", Annotations: creates},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Name    string `json:"name" jsonschema:"lowercase letters, digits and dashes"`
			Server  string `json:"server,omitempty"`
			Account string `json:"cloudflare_account,omitempty"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			name := strings.TrimSpace(in.Name)
			if _, err := ops.CreateProjectOn(s, mcpUserOf(ctx).ID, name, in.Server, in.Account); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "created project " + name, Next: "create_database if the app needs PostgreSQL, then create_app"}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "create_app", Description: "Create an app from a repository and start its first deploy: pushes to the default branch deploy it from then on. Take build_path and build_strategy from scan_repo. The app listens on the PORT variable it gets. Link a database, add volumes and set variables before (the first deploy may fail without them, then deploy again) or right after. Follow with wait_for_deploy.", Annotations: creates},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Project      string `json:"project"`
			Name         string `json:"name" jsonschema:"lowercase letters, digits and dashes; unique in its project"`
			Repo         string `json:"repo" jsonschema:"owner/name, from list_repos"`
			BuildPath    string `json:"build_path,omitempty" jsonschema:"from scan_repo; empty for the repo's root"`
			Strategy     string `json:"build_strategy,omitempty" jsonschema:"railpack (default) or dockerfile"`
			StartCommand string `json:"start_command,omitempty" jsonschema:"run with sh instead of what the image starts; usually leave it to Railpack"`
			Domain       string `json:"domain,omitempty" jsonschema:"its public address, a name in one of the project's domains (e.g. shop.example.com); empty for a private app"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			p, err := findMCPProject(ctx, s, mcpUserOf(ctx), in.Project)
			if err != nil {
				return nil, mcpDone{}, err
			}
			app := store.CreateAppParams{ProjectID: p.ID, Name: strings.TrimSpace(in.Name), Repo: strings.TrimSpace(in.Repo), BuildStrategy: in.Strategy}
			if err := ops.CheckRepo(s, mcpUserOf(ctx).GitHubID, app.Repo); err != nil {
				return nil, mcpDone{}, err
			}
			app.BuildPath = in.BuildPath
			if _, err := ops.CreateAppWith(s, app, strings.TrimSpace(in.Domain), in.StartCommand); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "created " + p.Name + "/" + app.Name + " and started its first deploy", Next: "wait_for_deploy, then get_app_logs"}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "create_database", Description: "Create a PostgreSQL database in a project and link it to an app there: the app gets DATABASE_URL and POSTGRES_* on its next deploy. Its password is generated and never shown. For SQLite use add_volume instead.", Annotations: creates},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Project string `json:"project"`
			Name    string `json:"name" jsonschema:"lowercase letters, digits, dashes and underscores"`
			App     string `json:"app,omitempty" jsonschema:"an app of the project to link it to, by its name there"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			p, err := findMCPProject(ctx, s, mcpUserOf(ctx), in.Project)
			if err != nil {
				return nil, mcpDone{}, err
			}
			name := strings.TrimSpace(in.Name)
			if _, err := ops.CreateDatabase(s, p.ID, name); err != nil {
				return nil, mcpDone{}, err
			}
			if in.App == "" {
				return nil, mcpDone{Done: "created database " + name, Next: "link_database to an app"}, nil
			}
			app, err := getApp(ctx, p.Name+"/"+in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.LinkDatabase(s, app, name); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "created database " + name + " and linked it to " + in.App, Next: "deploy " + in.App + " for it to get DATABASE_URL"}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "link_database", Description: "Link one of the project's databases to an app (\"\" unlinks): it gets DATABASE_URL and POSTGRES_* on its next deploy.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			App      string `json:"app" jsonschema:"project/app"`
			Database string `json:"database" jsonschema:"a database of the app's project, by its name there"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			app, err := getApp(ctx, in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.LinkDatabase(s, app, in.Database); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "linked", Next: "deploy " + in.App}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "create_cache", Description: "Create a cache in a project and link it to an app there: a Valkey server of its own (Redis-compatible, 256 MB), for caching, queues (Celery, BullMQ, Sidekiq) and sessions. The app gets REDIS_URL and REDIS_* on its next deploy. Its data outlives restarts but isn't backed up: keep nothing there that can't be rebuilt.", Annotations: creates},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Project string `json:"project"`
			Name    string `json:"name,omitempty" jsonschema:"lowercase letters, digits and dashes; empty for a suggested one"`
			App     string `json:"app,omitempty" jsonschema:"an app of the project to link it to, by its name there"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			p, err := findMCPProject(ctx, s, mcpUserOf(ctx), in.Project)
			if err != nil {
				return nil, mcpDone{}, err
			}
			id, err := ops.CreateCache(s, p.ID, strings.TrimSpace(in.Name))
			if err != nil {
				return nil, mcpDone{}, err
			}
			c, err := s.GetCacheByID(ctx, id)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if in.App == "" {
				return nil, mcpDone{Done: "created cache " + c.Name, Next: "link_cache to an app"}, nil
			}
			app, err := getApp(ctx, p.Name+"/"+in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.LinkCache(s, app, c.Name); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "created cache " + c.Name + " and linked it to " + in.App, Next: "deploy " + in.App + " for it to get REDIS_URL"}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "link_cache", Description: "Link one of the project's caches to an app (\"\" unlinks): it gets REDIS_URL and REDIS_* on its next deploy.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			App   string `json:"app" jsonschema:"project/app"`
			Cache string `json:"cache" jsonschema:"a cache of the app's project, by its name there"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			app, err := getApp(ctx, in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.LinkCache(s, app, in.Cache); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "linked", Next: "deploy " + in.App}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "add_volume", Description: "Give an app a volume: a directory kept across deploys and restarts, and backed up, for files like an SQLite database or uploads. An app with volumes stops its old version before starting a new one, so a deploy has a short downtime. Applies on the next deploy.", Annotations: creates},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			App       string `json:"app" jsonschema:"project/app"`
			Name      string `json:"name" jsonschema:"e.g. data"`
			MountPath string `json:"mount_path" jsonschema:"absolute path in the container, e.g. /data"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			app, err := getApp(ctx, in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.AddVolume(s, app.ID, strings.TrimSpace(in.Name), in.MountPath); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: "added volume " + in.Name + " at " + in.MountPath, Next: "point the app at it (e.g. set_env DATABASE_PATH=" + strings.TrimRight(in.MountPath, "/") + "/app.db), then deploy"}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_env", Description: "Set or remove some of an app's variables; the others stay. Only for settings that aren't secret: the values are visible in the panel and pass through this chat. For a secret (API key, password, token) don't ask the user for it here: tell them to add it under Sealed variables at the app's panel_url instead. A value can refer to another app of the project: ${{api.URL}} (its public address, for a browser), ${{api.HOST}}, ${{api.PRIVATE_URL}} (its address inside the project, for a server). VITE_, NEXT_PUBLIC_, PUBLIC_, REACT_APP_ and RAILPACK_ variables also reach the build. Applies on the next deploy.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			App   string            `json:"app" jsonschema:"project/app"`
			Set   map[string]string `json:"set,omitempty" jsonschema:"NAME: value"`
			Unset []string          `json:"unset,omitempty" jsonschema:"names to remove"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			app, err := getApp(ctx, in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.ChangeAppVars(s, app.ID, in.Set, in.Unset); err != nil {
				return nil, mcpDone{}, err
			}
			return nil, mcpDone{Done: fmt.Sprintf("set %d, removed %d", len(in.Set), len(in.Unset)), Next: "deploy " + in.App}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "set_route", Description: "Serve a path of an app's address by another app of its project, like a proxy in front of both: with path /api and target api, shop.example.com/api/... reaches api and the rest the shop. One address for a frontend and its API: no CORS, shared cookies. The target sees the whole path, /api included, and needs no address of its own. target \"\" removes the route. Applies within seconds.", Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			App    string `json:"app" jsonschema:"project/app, the app whose address it is"`
			Path   string `json:"path" jsonschema:"a prefix like /api"`
			Target string `json:"target" jsonschema:"another app of the project, by its name there; empty to remove the route"`
		}) (*mcp.CallToolResult, mcpDone, error) {
			if err := mayDeploy(req); err != nil {
				return nil, mcpDone{}, err
			}
			app, err := getApp(ctx, in.App)
			if err != nil {
				return nil, mcpDone{}, err
			}
			if err := ops.SetAppRoute(s, app, in.Path, in.Target); err != nil {
				return nil, mcpDone{}, err
			}
			if in.Target == "" {
				return nil, mcpDone{Done: "removed the route of " + in.Path}, nil
			}
			return nil, mcpDone{Done: in.App + in.Path + " goes to " + in.Target}, nil
		})
}
