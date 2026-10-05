package cmd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// iconSprite holds the panel's icons and logos as SVG symbols, put once on
// each page; the files' leading comments (their licenses) stay in them.
var (
	//go:embed icons.svg
	iconsFile string
	//go:embed logos.svg
	logosFile string
)

var iconSprite = func() string {
	_, icons, _ := strings.Cut(iconsFile, "-->")
	_, logos, _ := strings.Cut(logosFile, "-->")
	icons = strings.TrimSuffix(strings.TrimSpace(icons), "</svg>")
	return icons + strings.TrimSpace(logos) + "\n</svg>"
}()

// icon draws one icon of the sprite, with extra classes if given. A name
// that isn't in the sprite fails the page, so a typo shows up in tests.
func icon(name string, classes ...string) (template.HTML, error) {
	if !strings.Contains(iconSprite, `id="i-`+name+`"`) {
		return "", fmt.Errorf("no icon %q in icons.svg", name)
	}
	class := strings.TrimSpace("icon " + strings.Join(classes, " "))
	return template.HTML(`<svg class="` + template.HTMLEscapeString(class) + `" aria-hidden="true"><use href="#i-` + name + `"/></svg>`), nil
}

// The panel loads nothing from other sites: scripts, styles and fonts are
// in the binary. Pages are templ components (ui_*.templ); rebuild them and
// static/app.css with `go generate ./cmd` after changing either.
//
//go:generate go tool templ generate
//go:generate bunx --bun tailwindcss@3 -c tailwind.config.js -i styles.css -o static/app.css --minify
//go:embed static
var staticFiles embed.FS

// staticURL links a file in static/ with a hash of its content, so it can
// be cached for good and a new hakobu still loads its new version.
func staticURL(name string) string {
	b, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		panic(err) // a template names a file that isn't embedded
	}
	sum := sha256.Sum256(b)
	return "/static/" + name + "?v=" + hex.EncodeToString(sum[:6])
}

func staticHandler() http.Handler {
	files := http.FileServerFS(staticFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("v") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400") // fonts, linked from app.css
		}
		files.ServeHTTP(w, r)
	})
}

// Cookies use the __Host- prefix: apps live on sibling subdomains of the
// panel, and the prefix stops them from planting cookies for it.
const (
	sessionCookie    = "__Host-hakobu_session"
	setupCookie      = "__Host-hakobu_setup"
	manifestCookie   = "__Host-hakobu_gh_state"
	oauthStateCookie = "__Host-hakobu_oauth_state"
	afterCookie      = "__Host-hakobu_after" // where to go after signing in
	inviteCookie     = "__Host-hakobu_invite"
	sessionTTL       = 30 * 24 * time.Hour
	inviteTTL        = 7 * 24 * time.Hour
)

// keyFreshFor is how soon after signing in the master key can be
// downloaded; tests change it.
var keyFreshFor = 5 * time.Minute

// session is who an authed request comes from, and when they signed in;
// sessionKey holds it in the request's context.
type session struct {
	User     store.User
	SignedIn time.Time
}

type sessionKey struct{}

// requestUser is the signed-in user of an authed request.
func requestUser(r *http.Request) store.User {
	sess, _ := r.Context().Value(sessionKey{}).(session)
	return sess.User
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// panelHandler guards the panel against other sites. Apps are served from
// subdomains of the same domain, which SameSite cookies treat as the same
// site, so state-changing requests must also come from the panel's own
// origin. The webhook, the Sentry endpoint, OAuth's token endpoints and MCP
// are called cross-origin on purpose and check their own secrets.
func panelHandler(mux http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /webhook/github")
	cop.AddInsecureBypassPattern("POST /api/{app_id}/envelope/")
	// OAuth clients and MCP requests carry no cookies: they're authorized
	// by their code, refresh or access token.
	cop.AddInsecureBypassPattern("POST /oauth/token")
	cop.AddInsecureBypassPattern("POST /oauth/register")
	cop.AddInsecureBypassPattern("/mcp")
	h := cop.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", panelCSP("https://github.com"))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// panelCSP lets only the panel's own files run: no inline script and no
// eval. Forms may also go to formTarget: github.com, where the GitHub App
// manifest is posted, or the app an OAuth consent sends the browser back to.
func panelCSP(formTarget string) string {
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self' " + formTarget + "; frame-ancestors 'none'"
}

func cookieMatches(r *http.Request, name, want string) bool {
	c, err := r.Cookie(name)
	return err == nil && want != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// fail sends a plain-text error; the page shows it as a toast.
func fail(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusBadRequest)
}

// done tells htmx to reload the page, or to go to redirect if given.
func done(w http.ResponseWriter, redirect string) {
	if redirect != "" {
		w.Header().Set("HX-Redirect", redirect)
	} else {
		w.Header().Set("HX-Refresh", "true")
	}
}

func registerWebRoutes(mux *http.ServeMux, s *store.Store) {
	registerAuthRoutes(mux, s)
	registerOAuthRoutes(mux, s)
	registerMCPRoutes(mux, s)
	mux.Handle("GET /static/", staticHandler())
	// The watchdog's check, from outside: the panel answers, so the server,
	// hakobu and the tunnel are up. It tells nothing else to anyone.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, "ok")
	})

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if sess, ok := userSession(r, s); ok {
				r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess))
				if err := checkAccess(r, s); err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				h(w, r)
				return
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
	}
	handle := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, authed(h)) }
	registerUsageRoutes(handle, s)
	registerUptimeRoutes(handle, s)

	// action wraps a mutating handler: an error becomes a toast, success reloads the page.
	action := func(pattern string, h func(r *http.Request) (redirect string, err error)) {
		handle(pattern, func(w http.ResponseWriter, r *http.Request) {
			// A body that didn't arrive whole must not read as empty fields:
			// a missing "zone" would make an app private.
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if err := r.ParseForm(); err != nil {
				fail(w, err)
				return
			}
			redirect, err := h(r)
			if err != nil {
				fmt.Println(r.Method, r.URL.Path, "failed:", err)
				fail(w, err)
				return
			}
			done(w, redirect)
		})
	}

	// adminOnly guards what only the panel's admin may do: its updates,
	// backups, Cloudflare, notifications, secrets and users.
	adminOnly := func(h func(r *http.Request) (string, error)) func(r *http.Request) (string, error) {
		return func(r *http.Request) (string, error) {
			if requestUser(r).Admin != 1 {
				return "", errors.New("only the panel's admin can do that")
			}
			return h(r)
		}
	}

	// Users.

	handle("POST /settings/invites", func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r)
		if user.Admin != 1 {
			fail(w, errors.New("only the panel's admin can invite"))
			return
		}
		secret, err := ops.RandomHex(16)
		if err == nil {
			err = s.NewInvite(r.Context(), secret, user.ID, inviteTTL)
		}
		if err != nil {
			fail(w, err)
			return
		}
		renderPage(w, r, inviteLink("https://"+config.PublicHost()+"/invite/"+secret))
	})
	action("DELETE /settings/invites/{hash}", adminOnly(func(r *http.Request) (string, error) {
		return "", s.DeleteInvite(r.Context(), r.PathValue("hash"))
	}))
	action("POST /settings/users/{id}/admin", adminOnly(func(r *http.Request) (string, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return "", err
		}
		return "", ops.SetUserAdmin(s, id, r.FormValue("admin") == "1")
	}))
	action("POST /settings/my-notify", func(r *http.Request) (string, error) {
		return "", ops.SetUserNotify(s, requestUser(r), r.FormValue("email"))
	})
	action("POST /settings/my-notify/test", func(r *http.Request) (string, error) {
		return "", ops.SendUserTestEmail(s, requestUser(r))
	})
	action("DELETE /settings/users/{id}", adminOnly(func(r *http.Request) (string, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return "", err
		}
		return "", ops.RemoveUser(s, id)
	}))

	// Projects.

	handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		projects, err := s.ListProjectsOf(r.Context(), requestUser(r).ID)
		if err != nil {
			fail(w, err)
			return
		}
		// One container list per node, not one inspect per app.
		statuses := map[node.Node]map[string]string{}
		cards := make([]homeProject, 0, len(projects))
		for _, p := range projects {
			card := homeProject{Name: p.Name, Server: ops.ProjectServerName(s, p)}
			n := ops.ProjectNode(s, p.Name)
			if _, ok := statuses[n]; !ok {
				statuses[n], _ = n.Statuses(r.Context()) // nil if it doesn't answer: all down
			}
			apps, _ := s.ListAppsByProject(r.Context(), p.ID)
			addLogo := func(l string) {
				if l != "" && len(card.Logos) < 5 && !slices.Contains(card.Logos, l) {
					card.Logos = append(card.Logos, l)
				}
			}
			for _, a := range apps {
				addLogo(appLogo(a))
				card.Apps++
				if ops.IsDeploying(a.Name) {
					card.Deploying++
				} else if statuses[n][a.ContainerName()] != "running" {
					card.Down++
				}
			}
			dbs, _ := s.ListDatabasesByProject(r.Context(), p.ID)
			storages, _ := s.ListStoragesByProject(r.Context(), p.ID)
			card.Databases, card.Storages = len(dbs), len(storages)
			if len(dbs) > 0 {
				addLogo("postgresql")
			}
			cards = append(cards, card)
		}
		renderPage(w, r, homePage(cards, cloudflare.Lacking(ops.CachedTokenPermissions()), ops.ExpiringDomains(), placesOf(s, requestUser(r))))
	})

	action("POST /projects", func(r *http.Request) (string, error) {
		name := strings.TrimSpace(r.FormValue("name"))
		return "/projects/" + name, ops.CreateProjectOn(s, requestUser(r).ID, name, r.FormValue("server"), r.FormValue("client"))
	})

	action("POST /projects/{p}/server", func(r *http.Request) (string, error) {
		return "", ops.SetProjectServer(s, r.PathValue("p"), r.FormValue("server"))
	})

	projectPageHandler := func(tab string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, err := s.GetProject(r.Context(), r.PathValue("p"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			v := projectView{Project: p, Tab: tab, Workers: map[string]workerCard{}, LastDeploy: map[string]store.DeployLog{}, LastBackup: map[string]store.Backup{}}
			apps, _ := s.ListAppsByProject(r.Context(), p.ID)
			// Every tab's header counts the resources; the canvas also shows
			// their state: containers, last deploys and backups.
			canvas := tab == ""
			for _, a := range apps {
				av := appView{App: a}
				if canvas {
					av = newAppView(r.Context(), s, a)
				}
				v.Apps = append(v.Apps, av)
				if wk, err := s.GetWorker(r.Context(), a.Name); err == nil {
					card := workerCard{Name: wk.Name, Command: string(wk.Command)}
					if canvas {
						card.Status = ops.ProjectNode(s, p.Name).State(r.Context(), wk.ContainerName()).Status
					}
					v.Workers[a.Name] = card
				}
				if !canvas {
					continue
				}
				if logs := deploySummaries(r.Context(), s, a.Name, 1); len(logs) > 0 {
					v.LastDeploy[a.Name] = logs[0]
				}
			}
			v.Databases, _ = s.ListDatabasesByProject(r.Context(), p.ID)
			for _, d := range v.Databases {
				if !canvas {
					break
				}
				if list, _ := s.ListBackups(r.Context(), store.ListBackupsParams{Database: d.Name, Limit: 1}); len(list) > 0 {
					v.LastBackup[d.Name] = list[0]
				}
			}
			v.Storages, _ = s.ListStoragesByProject(r.Context(), p.ID)
			v.Volumes = map[string][]volumeCard{}
			for _, a := range apps {
				vols, _ := s.ListVolumes(r.Context(), a.Name)
				for _, vol := range vols {
					card := volumeCard{Volume: vol}
					if canvas {
						if list, _ := s.ListVolumeBackups(r.Context(), store.ListVolumeBackupsParams{AppName: a.Name, Volume: vol.Name, Limit: 1}); len(list) > 0 {
							card.Last, card.HasLast = list[0], true
						}
					}
					v.Volumes[a.Name] = append(v.Volumes[a.Name], card)
				}
			}
			v.Calls = appCalls(apps)
			v.Watchdog = ops.Watchdog(s).On
			v.Server, v.Servers = ops.ProjectServerName(s, p), joinedServers(s, requestUser(r))
			v.PanelOnly = config.PanelOnly
			if clients, err := ops.ClientAccounts(s); err == nil {
				for _, c := range clients {
					if c.UserID != p.UserID {
						continue
					}
					v.Clients = append(v.Clients, c.Name)
					if p.CloudflareAccountID.Valid && p.CloudflareAccountID.Int64 == c.ID {
						v.Client = c.Name
					}
				}
			}
			v.Sealed = ops.SealedKeys(s, "project", p.Name)
			v.SuggestedDB = ops.SuggestDatabaseName(s, p)
			v.SuggestedStorage = ops.SuggestStorageName(s, p)
			v.R2Off, v.R2URL = ops.ProjectR2Off(s, p)
			v.BackupBucket = ops.BackupBucket(s)
			renderPage(w, r, projectPage(v))
		}
	}
	// The top bar's menus: switch to another project, or to another app or
	// database of this one.
	handle("GET /switch/projects", func(w http.ResponseWriter, r *http.Request) {
		projects, err := s.ListProjectsOf(r.Context(), requestUser(r).ID)
		if err != nil {
			fail(w, err)
			return
		}
		renderPage(w, r, switchProjects(projects, r.URL.Query().Get("current")))
	})
	handle("GET /switch/resources", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.GetProject(r.Context(), r.URL.Query().Get("project"))
		if err != nil || p.UserID != requestUser(r).ID {
			http.NotFound(w, r)
			return
		}
		apps, _ := s.ListAppsByProject(r.Context(), p.ID)
		dbs, _ := s.ListDatabasesByProject(r.Context(), p.ID)
		storages, _ := s.ListStoragesByProject(r.Context(), p.ID)
		renderPage(w, r, switchResources(p.Name, apps, dbs, storages, r.URL.Query().Get("current")))
	})

	handle("GET /projects/{p}", projectPageHandler(""))
	for _, tab := range []string{"variables", "settings"} {
		handle("GET /projects/{p}/"+tab, projectPageHandler(tab))
	}

	action("DELETE /projects/{p}", func(r *http.Request) (string, error) {
		return "/", ops.DeleteProject(s, r.PathValue("p"))
	})

	action("POST /projects/{p}/account", func(r *http.Request) (string, error) {
		return "", ops.SetProjectAccount(s, r.PathValue("p"), r.FormValue("client"))
	})

	action("POST /projects/{p}/env", func(r *http.Request) (string, error) {
		return "", ops.SetSharedEnv(s, r.PathValue("p"), r.FormValue("env"))
	})

	handle("GET /projects/{p}/new-app", func(w http.ResponseWriter, r *http.Request) {
		repos, err := ops.ListRepos(s, requestUser(r).GitHubID)
		v := newAppData{Project: r.PathValue("p"), Repos: repos}
		v.Zones, v.DefaultZone, _ = ops.ProjectZones(s, v.Project)
		if err != nil {
			v.RepoError = err.Error()
		}
		if app, err := s.GetGitHubApp(r.Context()); err == nil {
			v.InstallURL = "https://github.com/apps/" + app.Slug + "/installations/new"
		}
		renderPage(w, r, newAppForm(v))
	})

	// The scan result is swapped into the new-app form, so errors are
	// rendered inline instead of as a toast.
	handle("POST /projects/{p}/apps/scan", func(w http.ResponseWriter, r *http.Request) {
		presets, err := ops.ScanRepoPresets(s, requestUser(r).GitHubID, strings.TrimSpace(r.FormValue("repo")))
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		renderPage(w, r, presetsFragment(presets, msg))
	})

	action("POST /projects/{p}/apps", func(r *http.Request) (string, error) {
		path, strategy, ok := strings.Cut(r.FormValue("preset"), "::")
		if !ok {
			return "", fmt.Errorf("pick a repository and how to build it")
		}
		app := store.CreateAppParams{
			Name:          strings.TrimSpace(r.FormValue("name")),
			Repo:          r.FormValue("repo"),
			BuildPath:     path,
			BuildStrategy: strategy,
		}
		if app.Repo != "" {
			if err := ops.CheckRepo(s, requestUser(r).GitHubID, app.Repo); err != nil {
				return "", err
			}
		}
		return "/apps/" + app.Name, ops.CreateApp(s, r.PathValue("p"), app, formDomain(r))
	})

	action("POST /projects/{p}/databases", func(r *http.Request) (string, error) {
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" { // the suggested name, shown as the placeholder
			p, err := s.GetProject(r.Context(), r.PathValue("p"))
			if err != nil {
				return "", err
			}
			if name = ops.SuggestDatabaseName(s, p); name == "" {
				return "", fmt.Errorf("type a name for the database")
			}
		}
		return "", ops.CreateDatabase(s, r.PathValue("p"), name)
	})

	action("POST /projects/{p}/storages", func(r *http.Request) (string, error) {
		return "", ops.CreateStorage(s, r.PathValue("p"), store.Storage{
			Name:            strings.TrimSpace(r.FormValue("name")),
			Provider:        r.FormValue("provider"),
			Endpoint:        strings.TrimSpace(r.FormValue("endpoint")),
			AccessKeyID:     strings.TrimSpace(r.FormValue("access_key_id")),
			SecretAccessKey: secret.String(strings.TrimSpace(r.FormValue("secret_access_key"))),
			Bucket:          strings.TrimSpace(r.FormValue("bucket")),
			Region:          strings.TrimSpace(r.FormValue("region")),
		})
	})

	action("POST /storages/{st}/keys", func(r *http.Request) (string, error) {
		return "", ops.SetStorageKeys(s, r.PathValue("st"), r.FormValue("access_key_id"), r.FormValue("secret_access_key"))
	})

	handle("GET /storages/{st}", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.GetStorage(r.Context(), r.PathValue("st"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		v := storagePage{Storage: st}
		v.Project, _ = s.GetProjectByID(r.Context(), st.ProjectID)
		v.UsedBy, _ = s.AppsUsingStorage(r.Context(), st.Name)
		renderPage(w, r, storageView(v))
	})

	action("DELETE /storages/{st}", func(r *http.Request) (string, error) {
		st, err := s.GetStorage(r.Context(), r.PathValue("st"))
		if err != nil {
			return "", err
		}
		redirect := "/"
		if p, err := s.GetProjectByID(r.Context(), st.ProjectID); err == nil {
			redirect = "/projects/" + p.Name
		}
		return redirect, ops.DeleteStorage(s, st.Name)
	})

	// Apps.

	getApp := func(w http.ResponseWriter, r *http.Request) (store.App, bool) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			http.NotFound(w, r)
		}
		return app, err == nil
	}

	// An app's pages: the header's data, then what the tab shows.
	appPageHandler := func(tab string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			app, ok := getApp(w, r)
			if !ok {
				return
			}
			p := appPage{App: newAppView(r.Context(), s, app), Tab: tab, DataRollbackBlocker: ops.DataRollbackBlocker(s, app), LastOOM: ops.LastOOM(s, app.Name)}
			p.Project, _ = s.GetProject(r.Context(), app.ProjectName)
			if wk, err := s.GetWorker(r.Context(), app.Name); err == nil {
				p.Worker = &wk
				p.WorkerStatus = ops.AppNode(s, app).State(r.Context(), wk.ContainerName()).Status
			}
			switch tab {
			case "overview":
				p.Deploys = deploySummaries(r.Context(), s, app.Name, 5)
				p.Events, _ = ops.AppEvents(s, app.Name, false, 5)
				renderPage(w, r, appOverview(p))
			case "deployments":
				var err error
				if p.Deploys, err = s.ListDeployLogs(r.Context(), store.ListDeployLogsParams{AppName: app.Name, Limit: 20}); err != nil {
					fail(w, err)
					return
				}
				renderPage(w, r, appDeployments(p))
			case "logs":
				container := app.ContainerName()
				if p.OfWorker = r.URL.Query().Get("worker") != "" && p.Worker != nil; p.OfWorker {
					container = app.Name + "-worker"
				}
				out, err := ops.AppNode(s, app).Logs(r.Context(), container, 300)
				if err != nil {
					p.OutputErr = err.Error()
				}
				p.Output = out
				renderPage(w, r, appLogs(p))
			case "errors":
				p.Logs = r.URL.Query().Get("logs") != ""
				var err error
				if p.Events, err = ops.AppEvents(s, app.Name, p.Logs, 50); err != nil {
					fail(w, err)
					return
				}
				renderPage(w, r, appErrors(p))
			case "performance":
				if p.Range = r.URL.Query().Get("range"); p.Range != "7d" {
					p.Range = "24h"
				}
				var err error
				if p.Routes, err = ops.RoutesOf(s, app.Name, ops.MetricRanges[p.Range]); err != nil {
					fail(w, err)
					return
				}
				if p.Traces, err = s.Tel.ListTraces(r.Context(), teldb.ListTracesParams{AppName: app.Name, Limit: 50}); err != nil {
					fail(w, err)
					return
				}
				renderPage(w, r, appPerformance(p))
			case "variables":
				env, err := ops.EffectiveEnv(s, app)
				if err != nil {
					p.EnvError = err.Error()
				}
				p.Effective = env
				p.SealedApp = ops.SealedKeys(s, "app", app.Name)
				p.SealedProject = ops.SealedKeys(s, "project", app.ProjectName)
				renderPage(w, r, appVariables(p))
			case "settings":
				p.Databases, _ = s.ListDatabasesByProject(r.Context(), app.ProjectID)
				p.Storages, _ = s.ListStoragesByProject(r.Context(), app.ProjectID)
				p.Zones, _, _ = ops.ProjectZones(s, app.ProjectName)
				p.Sub, p.Zone = splitDomain(app.Domain, p.Zones)
				p.Volumes, _ = s.ListVolumes(r.Context(), app.Name)
				for _, v := range p.Volumes {
					list, _ := s.ListVolumeBackups(r.Context(), store.ListVolumeBackupsParams{AppName: app.Name, Volume: v.Name, Limit: 10})
					job := ops.VolumeJob(app.Name, v.Name)
					p.VolumeBackups = append(p.VolumeBackups, volumeBackups{Volume: v.Name, Backups: list, Job: job})
					p.VolumeJobRunning = p.VolumeJobRunning || job.Running != ""
				}
				p.BackupBucket = ops.BackupBucket(s)
				p.SealedWorker = ops.SealedKeys(s, "worker", app.Name)
				renderPage(w, r, appSettings(p))
			}
		}
	}
	handle("GET /apps/{a}", appPageHandler("overview"))
	for _, tab := range []string{"deployments", "logs", "errors", "performance", "variables", "settings"} {
		handle("GET /apps/{a}/"+tab, appPageHandler(tab))
	}

	handle("GET /apps/{a}/deploys/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		d, err := s.GetDeployLog(r.Context(), store.GetDeployLogParams{ID: id, AppName: r.PathValue("a")})
		if err != nil {
			http.NotFound(w, r)
			return
		}
		renderPage(w, r, deployItem(d.AppName, d))
	})

	handle("GET /apps/{a}/status", func(w http.ResponseWriter, r *http.Request) {
		if app, ok := getApp(w, r); ok {
			renderPage(w, r, appStatus(newAppView(r.Context(), s, app)))
		}
	})

	action("POST /apps/{a}/deploy", func(r *http.Request) (string, error) {
		return "", ops.StartDeploy(s, r.PathValue("a"), "manual")
	})

	action("POST /apps/{a}/rollback", func(r *http.Request) (string, error) {
		return "", ops.StartRollback(s, r.PathValue("a"), r.FormValue("data") != "")
	})

	action("DELETE /apps/{a}", func(r *http.Request) (string, error) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			return "", err
		}
		return "/projects/" + app.ProjectName, ops.DeleteApp(s, app.Name)
	})

	action("POST /apps/{a}/settings", func(r *http.Request) (string, error) {
		name := r.PathValue("a")
		var port int64
		if v := strings.TrimSpace(r.FormValue("container_port")); v != "" {
			var err error
			if port, err = strconv.ParseInt(v, 10, 64); err != nil || port <= 0 || port > 65535 {
				return "", fmt.Errorf("port must be empty (detect automatically) or between 1 and 65535")
			}
		}
		app, err := s.GetApp(r.Context(), name)
		if err != nil {
			return "", err
		}
		if err := ops.SetAppDomain(s, app, formDomain(r)); err != nil {
			return "", err
		}
		return "", s.SetAppSettings(r.Context(), store.SetAppSettingsParams{
			Name:            name,
			ContainerPort:   port,
			HealthCheckPath: strings.TrimSpace(r.FormValue("health_check_path")),
		})
	})

	action("POST /apps/{a}/build", func(r *http.Request) (string, error) {
		return "", ops.SetAppBuild(s, r.PathValue("a"), r.FormValue("build_path"), r.FormValue("build_strategy"))
	})

	action("POST /apps/{a}/volumes", func(r *http.Request) (string, error) {
		return "", ops.AddVolume(s, r.PathValue("a"), strings.TrimSpace(r.FormValue("name")), r.FormValue("mount_path"))
	})

	action("DELETE /apps/{a}/volumes/{v}", func(r *http.Request) (string, error) {
		return "", ops.RemoveVolume(s, r.PathValue("a"), r.PathValue("v"))
	})

	action("POST /apps/{a}/volumes/{v}/backups", func(r *http.Request) (string, error) {
		return "", ops.StartVolumeBackup(s, r.PathValue("a"), r.PathValue("v"))
	})

	volumeBackupAction := func(pattern string, start func(*store.Store, string, string, int64) error) {
		action(pattern, func(r *http.Request) (string, error) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				return "", fmt.Errorf("no such backup")
			}
			return "", start(s, r.PathValue("a"), r.PathValue("v"), id)
		})
	}
	volumeBackupAction("POST /apps/{a}/volumes/{v}/backups/{id}/restore", ops.StartVolumeRestore)
	volumeBackupAction("POST /apps/{a}/volumes/{v}/backups/{id}/verify", ops.StartVolumeVerify)

	action("POST /apps/{a}/limits", func(r *http.Request) (string, error) {
		var memory int64
		var cpus float64
		var err error
		if v := strings.TrimSpace(r.FormValue("memory_mb")); v != "" {
			if memory, err = strconv.ParseInt(v, 10, 64); err != nil {
				return "", fmt.Errorf("memory limit must be a whole number of MB")
			}
		}
		if v := strings.TrimSpace(r.FormValue("cpus")); v != "" {
			if cpus, err = strconv.ParseFloat(v, 64); err != nil {
				return "", fmt.Errorf("CPU limit must be a number, e.g. 0.5")
			}
		}
		return "", ops.SetLimits(s, r.PathValue("a"), memory, cpus)
	})

	action("POST /apps/{a}/share-volumes", func(r *http.Request) (string, error) {
		return "", ops.SetShareVolumes(s, r.PathValue("a"), r.FormValue("share") != "")
	})

	action("POST /apps/{a}/links", func(r *http.Request) (string, error) {
		name := r.PathValue("a")
		if err := ops.LinkDatabase(s, name, r.FormValue("database")); err != nil {
			return "", err
		}
		return "", ops.LinkStorage(s, name, r.FormValue("storage"))
	})

	action("POST /apps/{a}/env", func(r *http.Request) (string, error) {
		return "", ops.SetAppEnv(s, r.PathValue("a"), r.FormValue("env"))
	})

	// Sealed variables of a project ("project", its name), app ("app") or
	// worker ("worker", its app's name).
	action("POST /sealed/{scope}/{owner}", func(r *http.Request) (string, error) {
		return "", ops.SealVar(s, r.PathValue("scope"), r.PathValue("owner"), strings.TrimSpace(r.FormValue("key")), r.FormValue("value"))
	})

	action("DELETE /sealed/{scope}/{owner}/{key}", func(r *http.Request) (string, error) {
		return "", ops.RemoveSealedVar(s, r.PathValue("scope"), r.PathValue("owner"), r.PathValue("key"))
	})

	handle("GET /apps/{a}/env/suggest", func(w http.ResponseWriter, r *http.Request) {
		app, ok := getApp(w, r)
		if !ok {
			return
		}
		file, keys := ops.FindEnvExampleKeys(s, app)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"file": file, "keys": keys})
	})

	handle("GET /apps/{a}/traces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		t, err := ops.TraceOf(s, r.PathValue("a"), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		renderPage(w, r, tracePage(traceView{t, time.Unix(t.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05 UTC")}))
	})

	// From an error to the trace of the request it happened in, if kept.
	handle("GET /apps/{a}/traces/by-trace/{trace}", func(w http.ResponseWriter, r *http.Request) {
		app := r.PathValue("a")
		id, err := s.Tel.TraceByTraceID(r.Context(), teldb.TraceByTraceIDParams{AppName: app, TraceID: r.PathValue("trace")})
		if err != nil {
			http.Error(w, "That request's trace wasn't kept: only slow and failed ones are, for a week, and only when the SDK traces requests (traces_sample_rate).", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/apps/%s/traces/%d", url.PathEscape(app), id), http.StatusSeeOther)
	})

	action("POST /apps/{a}/worker", func(r *http.Request) (string, error) {
		return "", ops.SaveWorker(s, store.Worker{
			AppName: r.PathValue("a"),
			Name:    strings.TrimSpace(r.FormValue("name")),
			Command: secret.String(strings.TrimSpace(r.FormValue("command"))),
			Env:     secret.String(r.FormValue("env")),
		})
	})

	action("POST /apps/{a}/worker/restart", func(r *http.Request) (string, error) {
		app, err := s.GetApp(r.Context(), r.PathValue("a"))
		if err != nil {
			return "", err
		}
		return "", ops.RestartWorker(s, app)
	})

	action("DELETE /apps/{a}/worker", func(r *http.Request) (string, error) {
		return "", ops.DeleteWorker(s, r.PathValue("a"))
	})

	// Databases.

	handle("GET /databases/{d}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.GetDatabase(r.Context(), r.PathValue("d"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		project, _ := s.GetProjectByID(r.Context(), d.ProjectID)
		backups, _ := s.ListBackups(r.Context(), store.ListBackupsParams{Database: d.Name, Limit: 30})
		usedBy, _ := s.AppsUsingDatabase(r.Context(), d.Name)
		renderPage(w, r, databaseView(databasePage{
			DB: d, Project: project, Ready: ops.DatabaseReady(s, d), Env: splitEnv(ops.DatabaseEnv(d)),
			Backups: backups, UsedBy: usedBy, BackupBucket: ops.BackupBucket(s),
			Job: ops.DatabaseJob(d.Name), Keep: config.BackupKeep,
		}))
	})

	action("DELETE /databases/{d}", func(r *http.Request) (string, error) {
		d, err := s.GetDatabase(r.Context(), r.PathValue("d"))
		if err != nil {
			return "", err
		}
		redirect := "/"
		if p, err := s.GetProjectByID(r.Context(), d.ProjectID); err == nil {
			redirect = "/projects/" + p.Name
		}
		return redirect, ops.DeleteDatabase(s, d.Name)
	})

	action("POST /databases/{d}/backups", func(r *http.Request) (string, error) {
		return "", ops.StartBackup(s, r.PathValue("d"))
	})

	backupAction := func(pattern string, start func(*store.Store, string, int64) error) {
		action(pattern, func(r *http.Request) (string, error) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				return "", fmt.Errorf("no such backup")
			}
			return "", start(s, r.PathValue("d"), id)
		})
	}
	backupAction("POST /databases/{d}/backups/{id}/restore", ops.StartRestore)
	backupAction("POST /databases/{d}/backups/{id}/verify", ops.StartVerify)

	// Settings.

	handle("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		user := requestUser(r)
		disk, diskLow := ops.DiskUsage()
		v := settingsPage{
			PublicHost: config.PublicHost(), User: user.GitHubLogin, Admin: user.Admin == 1,
			Disk: disk, DiskLow: diskLow, LastCleanup: ops.LastCleanup(),
			BackupBucket: ops.BackupBucket(s), CloudflareConnected: ops.CloudflareConnected(s),
			Rotation: ops.LastRotation(), PanelBackup: ops.LastPanelBackup(), KeyDownloaded: ops.KeyDownloaded(),
			Notify: ops.Notifications(s), Watchdog: ops.Watchdog(s), Update: ops.Updates(version),
			TokenURL: cloudflare.TokenTemplateURL("hakobu " + strings.Split(config.PublicHost(), ".")[0]),
		}
		if v.CloudflareConnected {
			v.Token = ops.TokenPermissions(s, false)
			clients, _ := ops.ClientAccounts(s)
			for _, c := range clients {
				if c.UserID == user.ID {
					v.Clients = append(v.Clients, c)
				}
			}
			ops.CheckClientsR2(s, v.Clients)
			v.R2Off, v.R2URL = ops.PanelR2Off(s)
			v.ClientTokenURL = ops.ClientTokenURL("client")
		}
		if usage, err := ops.CurrentUsage(s); err == nil {
			v.Usage = usageRows(usage)
		}
		servers, _ := ops.Servers(s)
		for _, sv := range servers {
			if sv.UserID == user.ID {
				v.Servers = append(v.Servers, sv)
			}
		}
		grants, _ := s.LiveOAuthGrants(r.Context())
		for _, g := range grants {
			if g.GitHubID == user.GitHubID {
				v.OAuthGrants = append(v.OAuthGrants, g)
			}
		}
		if app, err := s.GetGitHubApp(r.Context()); err == nil {
			v.GitHubSlug = app.Slug
		}
		v.MyNotify = ops.UserNotifyOf(s, user)
		if v.Admin {
			v.Users, _ = s.ListUsers(r.Context())
			v.Invites, _ = s.LiveInvites(r.Context())
		}
		renderPage(w, r, settingsView(v))
	})

	action("POST /settings/update/check", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.CheckForUpdates()
	}))

	action("POST /settings/update", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.RequestUpdate()
	}))

	action("POST /settings/rollback", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.RequestRollback()
	}))

	action("POST /settings/cleanup", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.Cleanup(s)
	}))

	// The join command is shown once, in place of the form.
	handle("POST /settings/servers", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.FormValue("name"))
		token, err := ops.AddServer(s, requestUser(r).ID, name)
		if err != nil {
			fail(w, err)
			return
		}
		renderPage(w, r, joinCommand(name, ops.JoinCommand(token)))
	})

	action("DELETE /settings/servers/{name}", func(r *http.Request) (string, error) {
		return "", ops.RemoveServer(s, r.PathValue("name"))
	})

	action("POST /settings/clients", func(r *http.Request) (string, error) {
		return "", ops.AddClientAccount(s, requestUser(r).ID, strings.TrimSpace(r.FormValue("name")), r.FormValue("token"))
	})

	action("POST /settings/clients/{c}/token", func(r *http.Request) (string, error) {
		return "", ops.ReplaceClientToken(s, r.PathValue("c"), r.FormValue("token"))
	})

	action("DELETE /settings/clients/{c}", func(r *http.Request) (string, error) {
		return "", ops.RemoveClientAccount(s, r.PathValue("c"))
	})

	action("POST /settings/backups", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.SetupBackups(s)
	}))

	action("POST /settings/notify", adminOnly(func(r *http.Request) (string, error) {
		email := r.FormValue("email")
		if email == "" {
			email = r.FormValue("other")
		}
		return "", ops.SetupNotifications(s, email, r.FormValue("name"), r.FormValue("from"))
	}))
	handle("GET /settings/notify/form", func(w http.ResponseWriter, r *http.Request) {
		if requestUser(r).Admin != 1 {
			http.NotFound(w, r)
			return
		}
		to, from, why, err := ops.NotifyChoices(s)
		n := ops.Notifications(s)
		name := n.Name
		if name == "" {
			name = ops.DefaultSenderName
		}
		v := notifyFormView{To: to, From: from, Why: why, PublicHost: config.PublicHost(), Notify: n, Name: name}
		if n.On && !slices.ContainsFunc(to, func(c ops.NotifyChoice) bool { return c.Selected }) {
			v.OtherValue = n.Email // not confirmed yet: it's no choice of its own
		}
		if err != nil {
			v.Error = err.Error()
		}
		renderPage(w, r, notifyForm(v))
	})
	action("POST /settings/watchdog", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.TurnWatchdogOn(s)
	}))

	action("DELETE /settings/watchdog", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.TurnWatchdogOff(s)
	}))

	action("POST /settings/cloudflare/token", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.ReplaceCloudflareToken(s, r.FormValue("token"))
	}))

	action("POST /settings/notify/test", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.SendTestEmail(s)
	}))
	action("DELETE /settings/notify", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.TurnOffNotifications(s)
	}))

	action("DELETE /settings/ai-apps/{id}", func(r *http.Request) (string, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return "", fmt.Errorf("no such app")
		}
		if g, err := s.GetOAuthGrant(r.Context(), id); err != nil || g.GitHubID != requestUser(r).GitHubID {
			return "", fmt.Errorf("no such app")
		}
		return "", s.DeleteOAuthGrant(r.Context(), id)
	})

	action("POST /settings/rotate-secrets", adminOnly(func(r *http.Request) (string, error) {
		return "", ops.StartRotation(s)
	}))

	// The master key opens every secret and backup, so it's handed out only
	// right after a GitHub sign-in, not to whoever finds a session open.
	handle("GET /settings/master-key", func(w http.ResponseWriter, r *http.Request) {
		if requestUser(r).Admin != 1 {
			http.NotFound(w, r)
			return
		}
		if sess, _ := r.Context().Value(sessionKey{}).(session); time.Since(sess.SignedIn) > keyFreshFor {
			setCookie(w, afterCookie, "master-key", 600)
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		kf, err := ops.CurrentKeyFile(s)
		if err != nil {
			fail(w, err)
			return
		}
		if err := ops.MarkKeyDownloaded(); err != nil {
			fmt.Println("failed to note the key download:", err)
		}
		host := strings.Map(func(r rune) rune {
			if r == '.' || r == '-' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
				return r
			}
			return -1
		}, strings.ToLower(config.PublicHost()))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="hakobu-master-key-`+host+`.txt"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(kf.Marshal())
	})
}

// registerAuthRoutes wires first-run setup and GitHub sign-in. The first
// person to sign in with the installer's setup link becomes the owner;
// after that only the owner can sign in.
func registerAuthRoutes(mux *http.ServeMux, s *store.Store) {
	setupAllowed := func(r *http.Request) bool {
		return cookieMatches(r, setupCookie, config.SetupToken())
	}

	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if admin, _ := s.Admin(r.Context()); admin.GitHubID != 0 {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if token := r.URL.Query().Get("token"); token != "" {
			want := config.SetupToken()
			if want == "" || subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
				http.Error(w, "invalid setup link", http.StatusForbidden)
				return
			}
			setCookie(w, setupCookie, token, 3600)
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		if !setupAllowed(r) {
			http.Error(w, "open the setup link printed by the installer (journalctl -u hakobu | grep setup)", http.StatusForbidden)
			return
		}
		if _, err := s.GetGitHubApp(r.Context()); err == nil {
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		var manifest, state string
		if host := config.PublicHost(); host != "" {
			m, err := github.BuildManifest(host)
			if err != nil {
				fail(w, err)
				return
			}
			state, _ = ops.RandomHex(16)
			setCookie(w, manifestCookie, state, 600)
			manifest = string(m)
		}
		renderPage(w, r, setupPage(config.PublicHost(), manifest, state))
	})

	mux.HandleFunc("GET /github-app/callback", func(w http.ResponseWriter, r *http.Request) {
		if admin, err := s.Admin(r.Context()); err != nil || admin.GitHubID != 0 {
			http.Error(w, "the panel already has an owner", http.StatusForbidden)
			return
		}
		if !cookieMatches(r, manifestCookie, r.URL.Query().Get("state")) || !setupAllowed(r) {
			http.Error(w, "invalid or expired setup session, open the setup link again", http.StatusBadRequest)
			return
		}
		setCookie(w, manifestCookie, "", -1)
		mc, err := github.ConvertManifestCode(r.URL.Query().Get("code"))
		if err != nil {
			fail(w, err)
			return
		}
		if err := s.SaveGitHubApp(r.Context(), store.SaveGitHubAppParams{
			AppID: mc.ID, Slug: mc.Slug, PrivateKey: secret.String(mc.PEM), WebhookSecret: secret.String(mc.WebhookSecret),
			ClientID: mc.ClientID, ClientSecret: secret.String(mc.ClientSecret),
		}); err != nil {
			fail(w, err)
			return
		}
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})

	// An invite link keeps its secret in a cookie for the sign-in, which
	// spends it when it makes the new user.
	mux.HandleFunc("GET /invite/{secret}", func(w http.ResponseWriter, r *http.Request) {
		setCookie(w, inviteCookie, r.PathValue("secret"), 3600)
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		_, err := s.GetGitHubApp(r.Context())
		renderPage(w, r, loginPage(err == nil, r.URL.Query().Get("error")))
	})

	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetGitHubApp(r.Context())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		state, _ := ops.RandomHex(16)
		setCookie(w, oauthStateCookie, state, 600)
		// Signing in again for the master key shows GitHub's account picker
		// instead of passing straight through.
		reauth := cookieMatches(r, afterCookie, "master-key")
		http.Redirect(w, r, github.AuthorizeURL(app.ClientID, "https://"+config.PublicHost()+"/auth/callback", state, reauth), http.StatusSeeOther)
	})

	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		deny := func(msg string) {
			http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusSeeOther)
		}
		if !cookieMatches(r, oauthStateCookie, r.URL.Query().Get("state")) {
			deny("sign-in expired, try again")
			return
		}
		setCookie(w, oauthStateCookie, "", -1)
		app, err := s.GetGitHubApp(r.Context())
		if err != nil {
			deny("GitHub is not connected yet")
			return
		}
		user, err := github.SignIn(app.ClientID, string(app.ClientSecret), r.URL.Query().Get("code"), "https://"+config.PublicHost()+"/auth/callback")
		if err != nil {
			fail(w, err)
			return
		}

		u, err := s.GetUserByGitHubID(r.Context(), user.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			admin, err := s.Admin(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			isAdmin := admin.GitHubID == 0
			switch c, cerr := r.Cookie(inviteCookie); {
			case isAdmin && !setupAllowed(r):
				deny("the panel has no admin yet, sign in through the installer's setup link")
				return
			case isAdmin:
			case cerr != nil:
				deny("GitHub account " + user.Login + " has no access to this panel: ask its admin for an invite")
				return
			default:
				setCookie(w, inviteCookie, "", -1)
				if err := s.UseInvite(r.Context(), c.Value); err != nil {
					deny(err.Error())
					return
				}
			}
			flag := int64(0)
			if isAdmin {
				flag = 1
			}
			id, err := s.CreateUser(r.Context(), store.CreateUserParams{GitHubID: user.ID, GitHubLogin: user.Login, Admin: flag})
			if err != nil {
				fail(w, err)
				return
			}
			if isAdmin {
				if err := config.ClearSetupToken(); err != nil {
					fmt.Println("failed to remove the setup token:", err)
				}
				setCookie(w, setupCookie, "", -1)
			}
			u = store.User{ID: id, GitHubID: user.ID, GitHubLogin: user.Login, Admin: flag}
		case err != nil:
			fail(w, err)
			return
		case u.GitHubLogin != user.Login:
			// They renamed their account; the panel shows the new name.
			if err := s.SetUserLogin(r.Context(), store.SetUserLoginParams{GitHubLogin: user.Login, ID: u.ID}); err != nil {
				fmt.Println("failed to update a user's login:", err)
			}
		}
		if user.Email != "" { // offered for notifications
			if err := s.SetUserEmail(r.Context(), store.SetUserEmailParams{GitHubEmail: user.Email, ID: u.ID}); err != nil {
				fmt.Println("failed to note a user's email:", err)
			}
		}

		id, err := ops.RandomHex(32)
		if err == nil {
			err = s.NewSession(r.Context(), id, user.ID, sessionTTL)
		}
		if err != nil {
			fail(w, err)
			return
		}
		setCookie(w, sessionCookie, id, int(sessionTTL.Seconds()))
		next := "/"
		if c, err := r.Cookie(afterCookie); err == nil {
			setCookie(w, afterCookie, "", -1)
			if c.Value == "master-key" {
				next = "/settings/master-key"
			} else if q, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c.Value, oauthAfterPrefix)); err == nil && strings.HasPrefix(c.Value, oauthAfterPrefix) {
				next = "/oauth/authorize?" + string(q)
			}
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
	})

	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		// Signing out must end the session on the server too: a copied
		// cookie would otherwise keep working.
		if c, err := r.Cookie(sessionCookie); err == nil {
			if err := s.EndSession(r.Context(), c.Value); err != nil {
				http.Error(w, "couldn't sign out: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		setCookie(w, sessionCookie, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

type appView struct {
	store.App
	node.State
	Deploying bool
}

// volumeBackups is one volume's part of the Backups card on an app's page.
type volumeBackups struct {
	Volume  string
	Backups []store.VolumeBackup
	Job     ops.DBJob
}

func newAppView(ctx context.Context, s *store.Store, a store.App) appView {
	return appView{App: a, State: ops.AppNode(s, a).State(ctx, a.ContainerName()), Deploying: ops.IsDeploying(a.Name)}
}

type envVar struct{ Key, Value string }

func splitEnv(env []string) []envVar {
	out := make([]envVar, 0, len(env))
	for _, line := range env {
		k, v, _ := strings.Cut(line, "=")
		out = append(out, envVar{k, v})
	}
	return out
}

// formDomain builds the domain from the "sub" and "zone" fields (an empty
// zone keeps the app private), or takes a plain "domain" field.
func formDomain(r *http.Request) string {
	_ = r.ParseForm() // a no-op after action, which checks its error
	if !r.Form.Has("zone") {
		return r.FormValue("domain")
	}
	zone := r.FormValue("zone")
	sub := strings.Trim(strings.TrimSpace(r.FormValue("sub")), ".")
	if zone == "" || sub == "" {
		return zone
	}
	return sub + "." + zone
}

// splitDomain is formDomain's inverse for the app settings form.
func splitDomain(domain string, zones []string) (sub, zone string) {
	for _, z := range zones {
		if domain == z {
			return "", z
		}
		if strings.HasSuffix(domain, "."+z) && len(z) > len(zone) {
			sub, zone = strings.TrimSuffix(domain, "."+z), z
		}
	}
	return sub, zone
}

// userSession reports who the request's session belongs to and when they
// signed in. The account is looked up on every request, so a user who was
// removed is out at once; their session is ended.
func userSession(r *http.Request, s *store.Store) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	id, signedIn, err := s.SessionUser(r.Context(), c.Value)
	if err != nil {
		return session{}, false
	}
	u, err := s.GetUserByGitHubID(r.Context(), id)
	if err == nil {
		return session{User: u, SignedIn: signedIn}, true
	}
	if err := s.EndSession(r.Context(), c.Value); err != nil {
		fmt.Println("failed to end a session:", err)
	}
	return session{}, false
}

// mayAccess reports whether a GitHub user ID is a user's.
func mayAccess(ctx context.Context, s *store.Store, githubID int64) bool {
	_, err := s.GetUserByGitHubID(ctx, githubID)
	return err == nil
}

// traceView is a kept trace for its page.
type traceView struct {
	ops.Waterfall
	At string
}

// appCalls guesses which apps of a project call which: an app whose own
// variables name another app's domain or its address on the project's
// network calls it. Shared variables are left out: every app gets them, so
// they say nothing about one.
func appCalls(apps []store.App) map[string][]string {
	calls := map[string][]string{}
	for _, a := range apps {
		env := strings.ToLower(string(a.Env))
		for _, b := range apps {
			if a.Name != b.Name && (namesHost(env, b.Domain) || namesHost(env, ops.EdgeAlias(b.Name))) {
				calls[a.Name] = append(calls[a.Name], b.Name)
			}
		}
	}
	return calls
}

// namesHost reports whether text has host as a whole name: api.example.com
// doesn't name example.com.
func namesHost(text, host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	isName := func(c byte) bool { return c == '.' || c == '-' || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') }
	for i := 0; ; {
		j := strings.Index(text[i:], host)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(host)
		if (start == 0 || !isName(text[start-1])) && (end == len(text) || !isName(text[end])) {
			return true
		}
		i = start + 1
	}
}

// deploySummaries is the app's last n deploys without their output, for
// where only their status and time show.
func deploySummaries(ctx context.Context, s *store.Store, app string, n int64) []store.DeployLog {
	rows, _ := s.ListDeploySummaries(ctx, store.ListDeploySummariesParams{AppName: app, Limit: n})
	logs := make([]store.DeployLog, len(rows))
	for i, d := range rows {
		logs[i] = store.DeployLog{ID: d.ID, AppName: d.AppName, Trigger: d.Trigger, Status: d.Status, CreatedAt: d.CreatedAt}
	}
	return logs
}

// joinedServers are the user's servers that have joined, to put projects
// on.
// placesOf are where the user's new projects can go.
func placesOf(s *store.Store, u store.User) projectPlaces {
	where := projectPlaces{Servers: joinedServers(s, u), PanelOnly: config.PanelOnly}
	clients, _ := ops.ClientAccounts(s)
	for _, c := range clients {
		if c.UserID == u.ID {
			where.Clients = append(where.Clients, c.Name)
		}
	}
	return where
}

func joinedServers(s *store.Store, u store.User) []string {
	servers, _ := ops.Servers(s)
	var names []string
	for _, sv := range servers {
		if sv.Joined && sv.UserID == u.ID {
			names = append(names, sv.Name)
		}
	}
	return names
}
