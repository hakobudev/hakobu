package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hako/internal/config"
	"github.com/x0ryz/hako/internal/deploy"
	"github.com/x0ryz/hako/internal/detect"
	"github.com/x0ryz/hako/internal/hostinfo"
	"github.com/x0ryz/hako/internal/ops"
	"github.com/x0ryz/hako/internal/store"
)

// registerWebRoutes wires the browser dashboard (htmx + Alpine.js + Tailwind,
// all via CDN — no build step) onto the agent's HTTP server. It's a thin
// server-rendered layer over the same internal/ops logic the /console-api JSON
// routes use, so there's exactly one place business rules live.
func registerWebRoutes(mux *http.ServeMux, s *store.Store, token string) {
	sessionCookie := "dt_session"

	// isFresh reports whether this looks like a first run: no projects and
	// no GitHub App yet. Only then is /setup served without a token — it's
	// the pre-login onboarding (public host + GitHub App), so a fresh
	// install on a remote server is configurable straight from the browser
	// instead of requiring SSH/env upfront. Binds stay on localhost by
	// default, so the window is LAN/SSH-tunnel only.
	isFresh := func() bool {
		if _, err := s.GetGitHubApp(); err == nil {
			return false
		}
		projects, err := s.ListProjects()
		return err != nil || len(projects) == 0
	}

	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if !isFresh() {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		render(w, "setup-page", map[string]string{"PublicHost": config.PublicBaseURL()})
	})

	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		if !isFresh() {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if host := r.FormValue("public_host"); host != "" {
			if err := config.SetPublicHost(host); err != nil {
				httpError(w, err)
				return
			}
		}
		if r.FormValue("app_id") != "" {
			if err := s.SaveGitHubApp(
				parseAppID(r.FormValue("app_id")),
				r.FormValue("slug"),
				r.FormValue("private_key"),
				r.FormValue("webhook_secret"),
				r.FormValue("client_id"),
				r.FormValue("client_secret"),
			); err != nil {
				httpError(w, err)
				return
			}
		}
		if isFresh() {
			render(w, "setup-page", map[string]string{"PublicHost": config.PublicBaseURL(), "Saved": "true"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(sessionCookie)
			if err != nil || c.Value != token {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		render(w, "login", nil)
	})

	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("token") != token {
			render(w, "login", map[string]string{"Error": "invalid token"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /{$}", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "dashboard", nil)
	}))

	mux.HandleFunc("GET /partials/server", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "server-card", buildHostView(s))
	}))

	mux.HandleFunc("GET /settings", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "settings-page", nil)
	}))

	buildDatabaseSettingsView := func() databaseSettingsView {
		view := databaseSettingsView{Versions: config.PostgresVersions}
		if ps, err := s.GetPostgresService(); err == nil {
			view.Postgres = ps
		}
		dbs, err := s.ListDatabases()
		if err != nil {
			fmt.Println("warning: failed to list databases:", err)
		} else {
			view.Databases = dbs
		}
		return view
	}

	mux.HandleFunc("GET /settings/database", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "settings-database-page", buildDatabaseSettingsView())
	}))

	mux.HandleFunc("POST /settings/database", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := ops.EnsurePostgresService(s, r.FormValue("version")); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-database-content", buildDatabaseSettingsView())
	}))

	mux.HandleFunc("POST /settings/databases", authed(func(w http.ResponseWriter, r *http.Request) {
		version := r.FormValue("version")
		if version == "" {
			version = config.PostgresVersions[0]
		}
		if _, err := ops.CreateDatabase(s, r.FormValue("name"), "postgres", version); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-database-content", buildDatabaseSettingsView())
	}))

	mux.HandleFunc("DELETE /settings/databases/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := ops.DeleteDatabase(s, r.PathValue("name")); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-database-content", buildDatabaseSettingsView())
	}))

	buildStorageSettingsView := func() storageSettingsView {
		view := storageSettingsView{}
		view.Storages, _ = ops.ListStorages(s)
		return view
	}

	mux.HandleFunc("GET /settings/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "settings-storage-page", buildStorageSettingsView())
	}))

	mux.HandleFunc("POST /settings/storages", authed(func(w http.ResponseWriter, r *http.Request) {
		st := store.Storage{
			Name:            r.FormValue("name"),
			Provider:        r.FormValue("provider"),
			AccountID:       r.FormValue("account_id"),
			Endpoint:        r.FormValue("endpoint"),
			AccessKeyID:     r.FormValue("access_key_id"),
			SecretAccessKey: r.FormValue("secret_access_key"),
			Bucket:          r.FormValue("bucket"),
			Region:          r.FormValue("region"),
		}
		if err := ops.CreateStorage(s, st); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-storage-content", buildStorageSettingsView())
	}))

	mux.HandleFunc("DELETE /settings/storages/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := ops.DeleteStorage(s, r.PathValue("name")); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-storage-content", buildStorageSettingsView())
	}))

	mux.HandleFunc("GET /settings/access", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "settings-access-page", buildAccessView(s))
	}))

	mux.HandleFunc("POST /settings/access", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := config.SetPublicHost(r.FormValue("public_host")); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-access-content", buildAccessView(s))
	}))

	mux.HandleFunc("GET /settings/github", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "settings-github-page", buildGitHubView(s))
	}))

	mux.HandleFunc("POST /settings/github", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := s.SaveGitHubApp(
			parseAppID(r.FormValue("app_id")),
			r.FormValue("slug"),
			r.FormValue("private_key"),
			r.FormValue("webhook_secret"),
			r.FormValue("client_id"),
			r.FormValue("client_secret"),
		); err != nil {
			httpError(w, err)
			return
		}
		render(w, "settings-github-content", buildGitHubView(s))
	}))

	renderProjectsPartial := func(w http.ResponseWriter, r *http.Request) {
		projects, err := s.ListProjects()
		if err != nil {
			httpError(w, err)
			return
		}
		var cards []projectCardView
		for _, p := range projects {
			cards = append(cards, buildProjectCard(r.Context(), s, p, false))
		}
		render(w, "projects-partial", projectsView{Cards: cards})
	}

	// renderProjectsListOOB refreshes the projects list out-of-band while
	// the actual hx response target is the (about-to-close) modal body —
	// so a failed create shows its error inside the modal instead of
	// wiping the whole list, which is what targeting #projects-tab
	// directly would do on error.
	renderProjectsListOOB := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<div id="projects-tab" hx-swap-oob="true">`)
		renderProjectsPartial(w, r)
		fmt.Fprint(w, `</div>`)
	}

	mux.HandleFunc("GET /partials/projects", authed(func(w http.ResponseWriter, r *http.Request) {
		renderProjectsPartial(w, r)
	}))

	mux.HandleFunc("GET /partials/new-project-form", authed(func(w http.ResponseWriter, r *http.Request) {
		render(w, "new-project-form", nil)
	}))

	mux.HandleFunc("POST /projects/scan", authed(func(w http.ResponseWriter, r *http.Request) {
		containerPort, _ := strconv.Atoi(r.FormValue("container_port"))
		if containerPort == 0 {
			containerPort = 8080
		}
		view := presetPickerView{
			Name:          r.FormValue("name"),
			Repo:          r.FormValue("repo"),
			Domain:        r.FormValue("domain"),
			ContainerPort: containerPort,
		}
		presets, err := ops.ScanRepoPresets(s, view.Repo)
		if err != nil {
			httpError(w, err)
			return
		}
		view.Presets = presets
		render(w, "preset-picker", view)
	}))

	mux.HandleFunc("POST /projects", authed(func(w http.ResponseWriter, r *http.Request) {
		containerPort, _ := strconv.Atoi(r.FormValue("container_port"))
		path, strategy, _ := strings.Cut(r.FormValue("preset"), "::")
		err := ops.CreateProject(s, r.FormValue("name"), r.FormValue("repo"), r.FormValue("domain"), containerPort, path, strategy)
		if err != nil {
			httpError(w, err)
			return
		}
		w.Header().Set("HX-Trigger", "project-created")
		renderProjectsListOOB(w, r)
	}))

	// renderProjectResult re-renders after an action on the app service —
	// as the full app-page content when the action came from that page
	// (?view=detail), or as the compact list row when it came from the
	// dashboard list, so each caller gets back exactly the fragment it's
	// holding. Database actions don't go through here — the database page
	// is self-contained (see renderProjectDatabase).
	renderProjectResult := func(w http.ResponseWriter, r *http.Request, name string) {
		project, err := s.GetProjectByName(name)
		if err != nil {
			httpError(w, err)
			return
		}
		detail := r.URL.Query().Get("view") == "detail"
		card := buildProjectCard(r.Context(), s, *project, false)
		if detail {
			render(w, "project-app-content", card)
			return
		}
		render(w, "project-row", card)
	}

	// GET /projects/{name} is the project canvas — a small architecture
	// view with one tile per resource (app, database, ...) that links to
	// that resource's own full page, rather than cramming every
	// resource's detail into one screen.
	mux.HandleFunc("GET /projects/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		project, err := s.GetProjectByName(r.PathValue("name"))
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-canvas-page", buildProjectCard(r.Context(), s, *project, true))
	}))

	mux.HandleFunc("GET /projects/{name}/app", authed(func(w http.ResponseWriter, r *http.Request) {
		project, err := s.GetProjectByName(r.PathValue("name"))
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-app-page", buildProjectCard(r.Context(), s, *project, false))
	}))

	mux.HandleFunc("GET /projects/{name}/logs", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		logs, err := s.ListDeployLogs(name, 10)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "deploy-logs", deployLogsView{Project: name, Logs: logs})
	}))

	mux.HandleFunc("GET /projects/{name}/errors", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		events, err := s.ListTelemetryEvents(name, "", 30)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "telemetry-events", telemetryEventsView{Project: name, Events: events})
	}))

	// buildProjectDatabaseView is shared by the database page and its own
	// htmx-driven refresh/create/delete actions.
	buildProjectDatabaseView := func(ctx context.Context, name string) (projectDatabaseView, error) {
		project, err := s.GetProjectByName(name)
		if err != nil {
			return projectDatabaseView{}, err
		}
		view := projectDatabaseView{ProjectName: project.Name}
		if project.LinkedDB != "" {
			if dbInfo, err := s.GetDatabase(project.LinkedDB); err == nil {
				cards := buildDatabaseCards(ctx, s, []store.Database{*dbInfo})
				view.HasDB = true
				view.DB = cards[0]
			}
		} else {
			view.Databases, _ = s.ListDatabases()
		}
		return view, nil
	}

	// renderProjectDatabase re-renders just the database fragment — used
	// by htmx for refresh/create/delete, all of which already live on the
	// database page and only need to replace that one block in place.
	renderProjectDatabase := func(w http.ResponseWriter, r *http.Request, name string) {
		view, err := buildProjectDatabaseView(r.Context(), name)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-database", view)
	}

	// GET /projects/{name}/database is the database's own full page for a
	// normal navigation (from the canvas tile), but htmx's own
	// refresh/create/delete calls hit the same URL and only want the
	// fragment back — distinguished by the header htmx always sends.
	mux.HandleFunc("GET /projects/{name}/database", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if r.Header.Get("HX-Request") == "true" {
			renderProjectDatabase(w, r, name)
			return
		}
		view, err := buildProjectDatabaseView(r.Context(), name)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-database-page", view)
	}))

	mux.HandleFunc("POST /projects/{name}/database/check", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if project, err := s.GetProjectByName(name); err == nil && project.LinkedDB != "" {
			if dbInfo, err := s.GetDatabase(project.LinkedDB); err == nil {
				ops.CheckDatabaseHealth(s, *dbInfo)
			}
		}
		renderProjectDatabase(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/database", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.LinkDatabase(s, name, r.FormValue("db_name")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectDatabase(w, r, name)
	}))

	mux.HandleFunc("DELETE /projects/{name}/database", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.UnlinkDatabase(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectDatabase(w, r, name)
	}))

	renderDatabaseBackups := func(w http.ResponseWriter, r *http.Request, projectName string) {
		project, err := s.GetProjectByName(projectName)
		if err != nil || project.LinkedDB == "" {
			httpError(w, fmt.Errorf("project %q has no database", projectName))
			return
		}
		db, err := s.GetDatabase(project.LinkedDB)
		if err != nil {
			httpError(w, err)
			return
		}
		backups, err := ops.ListBackups(s, project.LinkedDB)
		if err != nil {
			httpError(w, err)
			return
		}
		storages, err := ops.ListStorages(s)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "database-backups", databaseBackupsView{
			ProjectName:   projectName,
			DBName:        project.LinkedDB,
			Backups:       backups,
			Storages:      storages,
			BackupStorage: db.BackupStorage,
		})
	}

	mux.HandleFunc("GET /projects/{name}/database/backups", authed(func(w http.ResponseWriter, r *http.Request) {
		renderDatabaseBackups(w, r, r.PathValue("name"))
	}))

	mux.HandleFunc("POST /projects/{name}/database/backups", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		project, err := s.GetProjectByName(name)
		if err != nil || project.LinkedDB == "" {
			httpError(w, fmt.Errorf("project %q has no database", name))
			return
		}
		if _, _, err := ops.BackupDatabase(s, project.LinkedDB); err != nil {
			httpError(w, err)
			return
		}
		renderDatabaseBackups(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/database/backups/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		project, err := s.GetProjectByName(name)
		if err != nil || project.LinkedDB == "" {
			httpError(w, fmt.Errorf("project %q has no database", name))
			return
		}
		if err := ops.SetDatabaseBackupStorage(s, project.LinkedDB, r.FormValue("storage_name")); err != nil {
			httpError(w, err)
			return
		}
		renderDatabaseBackups(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/database/backups/restore", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		project, err := s.GetProjectByName(name)
		if err != nil || project.LinkedDB == "" {
			httpError(w, fmt.Errorf("project %q has no database", name))
			return
		}
		if err := ops.RestoreDatabase(s, project.LinkedDB, r.FormValue("object_key")); err != nil {
			httpError(w, err)
			return
		}
		renderDatabaseBackups(w, r, name)
	}))

	buildWorkerView := func(ctx context.Context, name string) workerView {
		view := workerView{ProjectName: name}
		if project, err := s.GetProjectByName(name); err == nil {
			view.SharedEnv = project.CustomEnv
		}
		if worker, err := s.GetWorker(name); err == nil {
			view.HasWorker = true
			view.Worker = buildWorkerCard(ctx, s, *worker)
		}
		return view
	}

	// renderProjectWorker re-renders just the worker fragment — same
	// self-contained pattern as renderProjectDatabase.
	renderProjectWorker := func(w http.ResponseWriter, r *http.Request, name string) {
		render(w, "project-worker", buildWorkerView(r.Context(), name))
	}

	// GET /projects/{name}/worker mirrors the database route: a full page
	// for a normal navigation, the bare fragment for htmx's own calls.
	mux.HandleFunc("GET /projects/{name}/worker", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if r.Header.Get("HX-Request") == "true" {
			renderProjectWorker(w, r, name)
			return
		}
		render(w, "project-worker-page", buildWorkerView(r.Context(), name))
	}))

	mux.HandleFunc("POST /projects/{name}/worker", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.CreateWorker(s, name, r.FormValue("name"), r.FormValue("command"), ""); err != nil {
			httpError(w, err)
			return
		}
		renderProjectWorker(w, r, name)
	}))

	mux.HandleFunc("DELETE /projects/{name}/worker", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.DeleteWorker(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectWorker(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/worker/redeploy", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := ops.RedeployWorker(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectWorker(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/worker/check", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		ops.CheckWorkerHealth(s, name)
		renderProjectWorker(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/worker/env", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.SetWorkerCustomEnv(s, name, r.FormValue("env")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectWorker(w, r, name)
	}))

	mux.HandleFunc("GET /projects/{name}/worker/container-logs", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		worker, err := s.GetWorker(name)
		if err != nil {
			httpError(w, err)
			return
		}
		logs, err := deploy.ContainerLogs(r.Context(), worker.ContainerName(), 200)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "container-logs", logs)
	}))

	buildProjectStorageView := func(name string) (projectStorageView, error) {
		project, err := s.GetProjectByName(name)
		if err != nil {
			return projectStorageView{}, err
		}
		storages, err := ops.ListStorages(s)
		if err != nil {
			return projectStorageView{}, err
		}
		return projectStorageView{ProjectName: name, LinkedStorage: project.LinkedStorage, Storages: storages}, nil
	}

	renderProjectStorage := func(w http.ResponseWriter, r *http.Request, name string) {
		view, err := buildProjectStorageView(name)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-storage", view)
	}

	mux.HandleFunc("GET /projects/{name}/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if r.Header.Get("HX-Request") == "true" {
			renderProjectStorage(w, r, name)
			return
		}
		view, err := buildProjectStorageView(name)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "project-storage-page", view)
	}))

	mux.HandleFunc("POST /projects/{name}/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.LinkStorage(s, name, r.FormValue("storage_name")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectStorage(w, r, name)
	}))

	mux.HandleFunc("DELETE /projects/{name}/storage", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.UnlinkStorage(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectStorage(w, r, name)
	}))

	mux.HandleFunc("GET /projects/{name}/container-logs", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		project, err := s.GetProjectByName(name)
		if err != nil {
			httpError(w, err)
			return
		}
		logs, err := deploy.ContainerLogs(r.Context(), project.ContainerName(), 200)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "container-logs", logs)
	}))

	mux.HandleFunc("POST /projects/{name}/check", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if project, err := s.GetProjectByName(name); err == nil {
			ops.CheckProjectHealth(s, *project)
		}
		renderProjectResult(w, r, name)
	}))

	mux.HandleFunc("DELETE /projects/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := ops.DeleteProject(s, r.PathValue("name")); err != nil {
			httpError(w, err)
			return
		}
		if r.URL.Query().Get("view") == "detail" {
			w.Header().Set("HX-Redirect", "/")
			return
		}
		renderProjectsPartial(w, r)
	}))

	mux.HandleFunc("POST /projects/{name}/redeploy", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := ops.RedeployProject(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectResult(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/rollback", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, err := ops.RollbackProject(s, name); err != nil {
			httpError(w, err)
			return
		}
		renderProjectResult(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/domain", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.SetProjectDomain(s, name, r.FormValue("domain")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectResult(w, r, name)
	}))

	mux.HandleFunc("POST /projects/{name}/health-check-path", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.SetProjectHealthCheckPath(s, name, r.FormValue("health_check_path")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectResult(w, r, name)
	}))

	// Shared variables live on the project canvas, not the app page — a
	// project-level setting shouldn't be nested one level down inside one
	// specific service's own page, even though the app happens to be the
	// service that reads it directly (the worker gets it passed in too).
	mux.HandleFunc("POST /projects/{name}/env", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.SetProjectCustomEnv(s, name, r.FormValue("env")); err != nil {
			httpError(w, err)
			return
		}
		project, err := s.GetProjectByName(name)
		if err != nil {
			httpError(w, err)
			return
		}
		render(w, "shared-env-form", project)
	}))

	mux.HandleFunc("POST /projects/{name}/app/env", authed(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := ops.SetProjectAppEnv(s, name, r.FormValue("env")); err != nil {
			httpError(w, err)
			return
		}
		renderProjectResult(w, r, name)
	}))

	// Backs the "found .env.example" banner in the env editor — checked
	// lazily from the browser (not on every page render) since it's a
	// live GitHub API round-trip. No example file, or no GitHub App
	// connected at all, both look the same to the caller: just no keys.
	mux.HandleFunc("GET /projects/{name}/env/suggest", authed(func(w http.ResponseWriter, r *http.Request) {
		project, err := s.GetProjectByName(r.PathValue("name"))
		if err != nil {
			httpError(w, err)
			return
		}
		file, keys := "", []string(nil)
		if project.Repo != "" {
			file, keys, _ = ops.FindEnvExampleKeys(s, project.Repo, project.BuildPath)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"file": file, "keys": keys})
	}))
}

func render(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func httpError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `<div class="text-sm text-bad">%s</div>`, template.HTMLEscapeString(err.Error()))
}

// pageHead: light theme, Vercel/Railway/Render-style dashboard chrome.
// Inter for UI text, JetBrains Mono for anything data-shaped (ports, env,
// logs) — the one deliberate typographic split, both loaded from the same
// CDN family as htmx/Alpine/Tailwind so there's still no build step.
const pageHead = `
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;650&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<script src="https://unpkg.com/htmx.org@2.0.3"></script>
<script defer src="https://unpkg.com/alpinejs@3.14.1/dist/cdn.min.js"></script>
<script src="https://cdn.tailwindcss.com"></script>
<meta name="viewport" content="width=device-width, initial-scale=1">
<script>
// Server reports validation/operation failures as 400s with an HTML error
// snippet as the body. htmx's default is to NOT swap non-2xx responses
// (it just fires htmx:responseError) — without this, a failed "Create
// project" silently does nothing visible instead of showing why.
htmx.config.responseHandling.unshift({code: "4..", swap: true});

// Backs every env-variable editor on the dashboard (shared/app/worker
// variables): a GitHub-Secrets-style list of rows by default, with a
// toggle to drop into a raw ".env" textarea for pasting, and both kept
// in sync through the same underlying "KEY=value" lines the server
// stores and renders back into the hidden seed textarea on save.
function envEditor() {
  return {
    mode: 'rows',
    rows: [],
    raw: '',
    suggestFile: '',
    suggestKeys: [],
    checked: false,
    init() {
      this.parse(this.$refs.seed.value);
    },
    parse(text) {
      this.rows = text.split('\n').map(l => l.trim()).filter(l => l.length > 0).map(line => {
        const i = line.indexOf('=');
        return i === -1
          ? {key: line, value: '', show: false}
          : {key: line.slice(0, i).trim(), value: line.slice(i + 1), show: false};
      });
    },
    serialize() {
      return this.rows.filter(r => r.key.trim() !== '').map(r => r.key.trim() + '=' + r.value).join('\n');
    },
    get serialized() {
      return this.mode === 'raw' ? this.raw : this.serialize();
    },
    toRaw() { this.raw = this.serialize(); this.mode = 'raw'; },
    toRows() { this.parse(this.raw); this.mode = 'rows'; },
    addRow() { this.rows.push({key: '', value: '', show: false}); },
    removeRow(i) { this.rows.splice(i, 1); },
    async checkExample(url) {
      if (this.checked || !url) return;
      this.checked = true;
      try {
        const res = await fetch(url, {credentials: 'same-origin'});
        if (!res.ok) return;
        const data = await res.json();
        const have = new Set(this.rows.map(r => r.key.trim()).filter(Boolean));
        this.suggestFile = data.file || '';
        this.suggestKeys = (data.keys || []).filter(k => !have.has(k));
      } catch (e) {}
    },
    addSuggestion(key) {
      this.rows.push({key, value: '', show: false});
      this.suggestKeys = this.suggestKeys.filter(k => k !== key);
    },
    addAllSuggestions() {
      for (const k of this.suggestKeys) this.rows.push({key: k, value: '', show: false});
      this.suggestKeys = [];
    },
  };
}
tailwind.config = {
  theme: {
    extend: {
      fontFamily: {
        sans: ['Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'monospace'],
      },
      colors: {
        canvas: '#ffffff',
        surface: '#ffffff',
        surface2: '#f4f4f5',
        edge: '#e4e4e7',
        ink: '#18181b',
        dim: '#71717a',
        faint: '#a1a1aa',
        accent: '#6d5ef7',
        dbaccent: '#0284c7',
        workeraccent: '#059669',
        storageaccent: '#d97706',
        good: '#16a34a',
        warn: '#d97706',
        bad: '#dc2626',
        console: '#0a0a0b',
        cink: '#d4d4d8',
        cdim: '#8b8894',
      },
    },
  },
}
</script>
<style>
  [x-cloak] { display: none !important; }
  body { font-family: 'Inter', ui-sans-serif, system-ui, sans-serif; font-feature-settings: "tnum" 1; }
  ::selection { background: rgba(109,94,247,0.18); }
  input, select { color-scheme: light; }
  input:focus, select:focus, textarea:focus {
    outline: none;
    box-shadow: 0 0 0 2px rgba(109,94,247,0.35);
  }
  a:focus-visible, button:focus-visible {
    outline: 2px solid #6d5ef7;
    outline-offset: 2px;
  }
</style>
`

// Shared utility class strings, so every card/panel/button/input across the
// dashboard renders from one definition instead of drifting apart one
// template literal at a time.
const (
	classCard      = "bg-surface border border-edge rounded-xl p-5"
	classCardTight = "bg-surface border border-edge rounded-xl p-4"
	// classConsole stays dark even in the light theme — raw log/env output
	// reads as a terminal in every one of the reference dashboards
	// (Vercel, Railway, Render), so it's the one deliberately inverted
	// surface rather than another neutral box stacked on white.
	classConsole   = "bg-console border border-edge rounded-lg px-3 py-2.5"
	classInput     = "bg-white border border-edge rounded-lg px-3 py-1.5 text-sm text-ink placeholder-faint outline-none focus:border-accent/50 transition-colors"
	classSelect    = classInput
	classBtnPri    = "text-sm font-medium bg-ink text-white rounded-lg px-3.5 py-1.5 hover:bg-zinc-800 transition-colors"
	classBtnSec    = "text-sm font-medium bg-white border border-edge text-ink rounded-lg px-3.5 py-1.5 hover:bg-surface2 transition-colors"
	classBtnGhost  = "text-xs text-dim hover:text-ink transition-colors"
	classBtnDanger = "text-sm font-medium text-bad border border-bad/20 rounded-lg px-3.5 py-1.5 hover:bg-bad/5 transition-colors"
	classMono      = "font-mono text-[13px]"
	classLabel     = "text-sm text-dim"
	classSubTab    = "pb-2.5 border-b-2 text-sm font-medium transition-colors"
	classSubTabOn  = "text-ink border-ink"
	classSubTabOff = "text-faint border-transparent hover:text-ink"
)

// iconAppLg/iconDBLg are small inline glyphs standing in for the app/
// database resource — a generic "service" bolt and a database cylinder
// (Lucide's bolt/database shapes, MIT-licensed) rather than exact
// third-party product logos, since a project's actual framework isn't
// tracked and every database here is Postgres.
const (
	iconAppLg     = `<svg viewBox="0 0 24 24" fill="currentColor" class="w-4 h-4"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg>`
	iconDBLg      = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="w-4 h-4"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14c0 1.7 4 3 9 3s9-1.3 9-3V5"/><path d="M3 12c0 1.7 4 3 9 3s9-1.3 9-3"/></svg>`
	iconWorkerLg  = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="w-4 h-4"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/></svg>`
	iconStorageLg = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="w-4 h-4"><path d="M21 8a2 2 0 0 0-1-1.7l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.7l7 4a2 2 0 0 0 2 0l7-4a2 2 0 0 0 1-1.7z"/><path d="M3.3 7 12 12l8.7-5"/><path d="M12 22V12"/></svg>`
)

// canvasTile is the shared look for a resource block on the project
// canvas — a clickable card with an icon, a label, and a status line, so
// adding a future resource kind (worker, object storage, ...) is just
// another <a> with this class plus a matching page/route, same shape as
// the app/database tiles that already exist.
const canvasTile = "flex items-center gap-2.5 bg-white border border-edge rounded-xl px-4 py-3 transition-all"

// All page/partial templates live in one tree so {{template "project-row"}}
// can be shared between the projects list and single-card re-renders after
// an htmx action.
const templatesSrc = `
{{define "login"}}<!doctype html>
<html><head><title>Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen flex items-center justify-center antialiased">
  <form method="post" action="/login" class="` + classCard + ` w-full max-w-sm space-y-4">
    <div class="flex items-center gap-2">
      <span class="w-2 h-2 rounded-full bg-accent"></span>
      <h1 class="text-[15px] font-semibold tracking-tight">Hako</h1>
    </div>
    {{if .Error}}<p class="text-sm text-bad">{{.Error}}</p>{{end}}
    <input type="password" name="token" placeholder="API token" autofocus class="` + classInput + ` w-full">
    <button class="w-full ` + classBtnPri + `">Sign in</button>
  </form>
</body></html>{{end}}

{{define "page-header"}}
<div class="flex items-center justify-between mb-8 pb-4 border-b border-edge">
  <a href="/" class="flex items-center gap-2 hover:opacity-70 transition-opacity">
    <span class="w-2 h-2 rounded-full bg-accent"></span>
    <h1 class="text-[15px] font-semibold tracking-tight">Hako</h1>
  </a>
  <div class="flex items-center gap-4">
    <a href="/settings" class="text-sm text-faint hover:text-ink transition-colors">Settings</a>
    <a href="/logout" class="text-sm text-faint hover:text-ink transition-colors">Sign out</a>
  </div>
</div>
{{end}}

{{define "dashboard"}}<!doctype html>
<html><head><title>Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8" x-data="{newProjectOpen:false}">
    {{template "page-header"}}

    <div id="server-card" hx-get="/partials/server" hx-trigger="load, every 10s"></div>

    <div class="flex items-center justify-between mb-4">
      <h2 class="text-sm font-medium text-dim">Projects</h2>
      <button @click="newProjectOpen=true"
        hx-get="/partials/new-project-form" hx-target="#new-project-modal-body" class="` + classBtnPri + `">
        New project
      </button>
    </div>

    <div id="projects-tab" hx-get="/partials/projects" hx-trigger="load" class="space-y-2"></div>

    <div x-show="newProjectOpen" x-cloak @project-created.window="newProjectOpen=false" @keydown.escape.window="newProjectOpen=false"
      class="fixed inset-0 z-50 flex items-start justify-center p-4 pt-24">
      <div class="absolute inset-0 bg-ink/25" @click="newProjectOpen=false"></div>
      <div class="relative ` + classCard + ` w-full max-w-md space-y-4 shadow-xl">
        <div class="flex items-center justify-between">
          <h2 class="font-medium">New project</h2>
          <button @click="newProjectOpen=false" class="text-faint hover:text-ink transition-colors" aria-label="Close">✕</button>
        </div>
        <div id="new-project-modal-body"></div>
      </div>
    </div>
  </div>
</body></html>{{end}}

{{define "server-card"}}
<div id="server-card" class="` + classCard + ` mb-8">
  <div class="flex items-center justify-between mb-4">
    <h2 class="text-sm font-medium text-dim">Server</h2>
    {{if .HasData}}
    <span class="inline-flex items-center gap-1.5 text-xs text-good"><span class="w-1.5 h-1.5 rounded-full bg-good"></span>online</span>
    {{end}}
  </div>
  {{if .HasData}}
  <div class="grid grid-cols-2 sm:grid-cols-4 gap-6">
    <div>
      <p class="text-xs text-faint mb-1">CPU</p>
      <p class="text-lg font-medium ` + classMono + `">{{printf "%.0f" .CPUPercent}}%</p>
      <div class="mt-1.5">{{.CPUSpark}}</div>
    </div>
    <div>
      <p class="text-xs text-faint mb-1">Memory</p>
      <p class="text-lg font-medium ` + classMono + `">{{printf "%.1f" .MemUsedGB}} <span class="text-faint text-sm">/ {{printf "%.0f" .MemTotalGB}} GB</span></p>
      <div class="mt-1.5">{{.MemSpark}}</div>
    </div>
    <div>
      <p class="text-xs text-faint mb-1">Disk</p>
      <p class="text-lg font-medium ` + classMono + `">{{printf "%.0f" .DiskUsedGB}} <span class="text-faint text-sm">/ {{printf "%.0f" .DiskTotalGB}} GB</span></p>
    </div>
    <div>
      <p class="text-xs text-faint mb-1">Load avg</p>
      <p class="text-lg font-medium ` + classMono + `">{{printf "%.2f" .Load1}} <span class="text-faint text-sm">{{printf "%.2f" .Load5}} · {{printf "%.2f" .Load15}}</span></p>
    </div>
  </div>
  {{else}}
  <p class="text-sm text-faint">Server metrics unavailable</p>
  {{end}}
</div>
{{end}}

{{define "project-row"}}
<div id="project-{{.Project.Name}}" class="flex items-center justify-between gap-3 px-4 py-3.5 border border-edge rounded-xl bg-white hover:border-zinc-300 transition-colors">
  <a href="/projects/{{.Project.Name}}" class="flex items-center gap-3 min-w-0 flex-1">
    <span class="w-1.5 h-1.5 rounded-full shrink-0 {{statusColor .Status}}"></span>
    <span class="font-medium truncate">{{.Project.Name}}</span>
    {{if .Responding}}
    <span class="text-xs shrink-0 {{if eq .Responding "responding"}}text-good{{else}}text-bad{{end}}">{{.Responding}}</span>
    {{end}}
    <span class="` + classMono + ` text-xs text-faint truncate hidden sm:inline">
      {{if .Project.Domain}}{{.Project.Domain}}{{else}}no domain{{end}}
    </span>
  </a>
  <div class="flex items-center gap-2 shrink-0">
    <button hx-post="/projects/{{.Project.Name}}/redeploy" hx-target="#project-{{.Project.Name}}" hx-swap="outerHTML" class="` + classBtnSec + `">
      Redeploy
    </button>
    <button hx-delete="/projects/{{.Project.Name}}" hx-target="#projects-tab" hx-swap="innerHTML"
      hx-confirm="Delete project {{.Project.Name}}? Its container will be stopped and removed."
      class="` + classBtnDanger + `">
      Delete
    </button>
  </div>
</div>
{{end}}

{{define "project-canvas-page"}}<!doctype html>
<html><head><title>{{.Project.Name}} · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-5xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <div class="flex flex-wrap items-center justify-between gap-3 mb-8">
      <div class="min-w-0">
        <h1 class="text-xl font-semibold truncate">{{.Project.Name}}</h1>
        <p class="text-xs text-faint ` + classMono + ` truncate">{{.Project.Repo}}</p>
      </div>
      <button hx-delete="/projects/{{.Project.Name}}?view=detail"
        hx-confirm="Delete project {{.Project.Name}}? Its container(s) will be stopped and removed."
        class="` + classBtnDanger + `">
        Delete project
      </button>
    </div>

    <div class="rounded-2xl border border-edge p-10 sm:p-20" style="background-image: radial-gradient(#e4e4e7 1px, transparent 1px); background-size: 20px 20px;">
      <div class="flex flex-wrap items-center justify-center gap-4">
        <a href="/projects/{{.Project.Name}}/app" class="` + canvasTile + ` hover:border-accent/50 hover:shadow-sm">
          <span class="w-9 h-9 rounded-lg bg-accent/10 text-accent border border-accent/20 flex items-center justify-center shrink-0">` + iconAppLg + `</span>
          <span>
            <span class="block text-sm font-medium">App</span>
            <span class="flex items-center gap-1.5 text-xs text-dim">
              <span class="w-1.5 h-1.5 rounded-full {{statusColor .Status}}"></span>{{.Status}}
            </span>
          </span>
        </a>

        <span class="w-8 border-t border-dashed border-faint/60 shrink-0"></span>

        {{if .HasDB}}
        <a href="/projects/{{.Project.Name}}/database" class="` + canvasTile + ` hover:border-dbaccent/50 hover:shadow-sm">
          <span class="w-9 h-9 rounded-lg bg-dbaccent/10 text-dbaccent border border-dbaccent/20 flex items-center justify-center shrink-0">` + iconDBLg + `</span>
          <span>
            <span class="block text-sm font-medium">{{.DB.Database.Name}}</span>
            <span class="flex items-center gap-1.5 text-xs text-dim">
              <span class="w-1.5 h-1.5 rounded-full {{if .DB.Ready}}bg-good{{else}}bg-bad{{end}}"></span>{{if .DB.Ready}}ready{{else}}not responding{{end}}
            </span>
          </span>
        </a>
        {{else}}
        <a href="/projects/{{.Project.Name}}/database" class="flex items-center gap-2.5 border border-dashed border-edge rounded-xl px-4 py-3 text-faint hover:border-zinc-400 hover:text-ink transition-colors">
          <span class="text-sm">+ Add database</span>
        </a>
        {{end}}

        <span class="w-8 border-t border-dashed border-faint/60 shrink-0"></span>

        {{if .HasWorker}}
        <a href="/projects/{{.Project.Name}}/worker" class="` + canvasTile + ` hover:border-workeraccent/50 hover:shadow-sm">
          <span class="w-9 h-9 rounded-lg bg-workeraccent/10 text-workeraccent border border-workeraccent/20 flex items-center justify-center shrink-0">` + iconWorkerLg + `</span>
          <span>
            <span class="block text-sm font-medium">{{.Worker.Worker.Name}}</span>
            <span class="flex items-center gap-1.5 text-xs text-dim">
              <span class="w-1.5 h-1.5 rounded-full {{if .Worker.Up}}bg-good{{else}}bg-bad{{end}}"></span>{{if .Worker.Up}}running{{else}}not running{{end}}
            </span>
          </span>
        </a>
        {{else}}
        <a href="/projects/{{.Project.Name}}/worker" class="flex items-center gap-2.5 border border-dashed border-edge rounded-xl px-4 py-3 text-faint hover:border-zinc-400 hover:text-ink transition-colors">
          <span class="text-sm">+ Add worker</span>
        </a>
        {{end}}

        <span class="w-8 border-t border-dashed border-faint/60 shrink-0"></span>

        {{if .Project.LinkedStorage}}
        <a href="/projects/{{.Project.Name}}/storage" class="` + canvasTile + ` hover:border-storageaccent/50 hover:shadow-sm">
          <span class="w-9 h-9 rounded-lg bg-storageaccent/10 text-storageaccent border border-storageaccent/20 flex items-center justify-center shrink-0">` + iconStorageLg + `</span>
          <span>
            <span class="block text-sm font-medium">{{.Project.LinkedStorage}}</span>
            <span class="flex items-center gap-1.5 text-xs text-dim">linked</span>
          </span>
        </a>
        {{else}}
        <a href="/projects/{{.Project.Name}}/storage" class="flex items-center gap-2.5 border border-dashed border-edge rounded-xl px-4 py-3 text-faint hover:border-zinc-400 hover:text-ink transition-colors">
          <span class="text-sm">+ Add storage</span>
        </a>
        {{end}}
      </div>
    </div>

    <div class="mt-6 ` + classCard + `">
      {{template "shared-env-form" .Project}}
    </div>

    <div class="mt-6 ` + classCard + `">
      {{template "effective-env" .}}
    </div>
  </div>
</body></html>{{end}}

{{/* effective-env shows the read-only, fully-resolved env a container
     actually runs with, row per variable like env-editor's list — but with
     no inputs (nothing here is editable, it's computed) and values hidden
     behind a per-row show/hide toggle by default, same as a real secrets
     manager, since this includes the database password and the Sentry DSN
     key. */}}
{{define "effective-env"}}
<div class="space-y-1.5">
  <div class="flex items-center justify-between">
    <span class="text-sm font-medium">All variables (effective)</span>
    <span class="text-xs text-faint">database + shared + SENTRY_DSN + app — what the container actually runs with, once (re)deployed</span>
  </div>
  <div class="space-y-1.5">
    {{range .EffectiveEnv}}
    <div class="flex gap-1.5 items-center" x-data="{show:false}">
      <span class="` + classInput + ` ` + classMono + ` w-2/5 py-1 truncate" title="{{.Key}}">{{.Key}}</span>
      <span class="` + classInput + ` ` + classMono + ` flex-1 py-1 truncate" x-show="show" x-cloak title="{{.Value}}">{{.Value}}</span>
      <span class="` + classInput + ` ` + classMono + ` flex-1 py-1 truncate text-faint" x-show="!show">••••••••••••</span>
      <button type="button" @click="show = !show" class="` + classBtnGhost + `" x-text="show ? 'hide' : 'show'"></button>
    </div>
    {{else}}
    <p class="text-xs text-faint">(nothing yet — deploy the project once)</p>
    {{end}}
  </div>
</div>
{{end}}

{{/* env-editor renders a GitHub-Secrets-style key/value list on top of the
     same "KEY=value\n..." text the server stores, with a toggle to paste a
     raw .env block instead, and a banner offering keys pulled from a
     .env.example-style file in the repo (fetched lazily, client-side, from
     .SuggestURL). Takes: {Value string, SuggestURL string}. */}}
{{define "env-editor"}}
<div x-data="envEditor()" data-suggest-url="{{.SuggestURL}}" x-init="init(); checkExample($el.dataset.suggestUrl)">
  <textarea x-ref="seed" hidden>{{.Value}}</textarea>
  <input type="hidden" name="env" :value="serialized">

  <div x-show="mode==='rows' && suggestKeys.length" x-cloak class="flex flex-wrap items-center gap-2 mb-2 text-xs bg-accent/5 border border-accent/20 rounded-lg px-3 py-2">
    <span class="text-dim">Found <span x-text="suggestKeys.length"></span> variable(s) in <span x-text="suggestFile" class="` + classMono + `"></span></span>
    <button type="button" @click="addAllSuggestions()" class="text-accent font-medium hover:underline">Add all</button>
    <template x-for="k in suggestKeys" :key="k">
      <button type="button" @click="addSuggestion(k)" class="` + classMono + ` bg-white border border-edge rounded px-1.5 py-0.5 hover:border-accent/50" x-text="k"></button>
    </template>
  </div>

  <div x-show="mode==='rows'" class="space-y-1.5">
    <template x-for="(row, i) in rows" :key="i">
      <div class="flex gap-1.5 items-center">
        <input type="text" x-model="row.key" placeholder="KEY" class="` + classInput + ` ` + classMono + ` w-2/5 py-1">
        <input :type="row.show ? 'text' : 'password'" x-model="row.value" placeholder="value" class="` + classInput + ` ` + classMono + ` flex-1 py-1">
        <button type="button" @click="row.show = !row.show" class="` + classBtnGhost + `" x-text="row.show ? 'hide' : 'show'"></button>
        <button type="button" @click="removeRow(i)" class="` + classBtnGhost + ` hover:text-bad" aria-label="Remove">✕</button>
      </div>
    </template>
    <button type="button" @click="addRow()" class="` + classBtnGhost + `">+ Add variable</button>
  </div>

  <textarea x-show="mode==='raw'" x-cloak x-model="raw" rows="6" placeholder="KEY=value" class="` + classInput + ` w-full ` + classMono + `"></textarea>

  <div class="mt-1.5 text-right">
    <button type="button" @click="mode==='rows' ? toRaw() : toRows()" class="` + classBtnGhost + `">
      <span x-text="mode==='rows' ? 'Edit as .env file' : 'Back to list'"></span>
    </button>
  </div>
</div>
{{end}}

{{define "shared-env-form"}}
<form id="shared-env-form" hx-post="/projects/{{.Name}}/env" hx-target="#shared-env-form" hx-swap="outerHTML" class="space-y-2">
  <div class="flex items-center justify-between">
    <span class="text-sm font-medium">Shared variables</span>
    <span class="text-xs text-faint">applies on next redeploy — passed to the app and its worker</span>
  </div>
  {{template "env-editor" (dict "Value" .CustomEnv "SuggestURL" (printf "/projects/%s/env/suggest" .Name))}}
  <button class="` + classBtnSec + `">Save</button>
</form>
{{end}}

{{define "project-app-content"}}
<div id="project-app-content" x-data="{tab:'overview'}">
  <a href="/projects/{{.Project.Name}}" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← {{.Project.Name}}</a>

  <div class="flex flex-wrap items-center justify-between gap-3 mb-6">
    <div class="flex items-center gap-3 min-w-0">
      <span class="w-8 h-8 rounded-lg bg-accent/10 text-accent border border-accent/20 flex items-center justify-center shrink-0">` + iconAppLg + `</span>
      <div class="min-w-0">
        <div class="flex items-center gap-2">
          <h1 class="text-lg font-semibold">App</h1>
          <span class="inline-flex items-center gap-1.5 text-xs text-dim shrink-0">
            <span class="w-1.5 h-1.5 rounded-full {{statusColor .Status}}"></span>{{.Status}}
            {{if .Responding}}<span class="{{if eq .Responding "responding"}}text-good{{else}}text-bad{{end}}">· {{.Responding}}</span>{{end}}
          </span>
        </div>
        <p class="text-xs text-faint ` + classMono + ` truncate">{{.Project.Repo}}</p>
      </div>
    </div>
    <div class="flex gap-2 shrink-0">
      <button hx-post="/projects/{{.Project.Name}}/rollback?view=detail" hx-target="#project-app-content" hx-swap="outerHTML"
        hx-confirm="Roll back {{.Project.Name}} to the image running before its last build?" class="` + classBtnGhost + `">
        Rollback
      </button>
      <button hx-post="/projects/{{.Project.Name}}/redeploy?view=detail" hx-target="#project-app-content" hx-swap="outerHTML" class="` + classBtnSec + `">
        Redeploy
      </button>
    </div>
  </div>

  <div class="flex gap-5 border-b border-edge mb-6 overflow-x-auto">
    <button type="button" @click="tab='overview'" :class="tab==='overview' ? '` + classSubTabOn + `' : '` + classSubTabOff + `'" class="` + classSubTab + ` shrink-0">Overview</button>
    <button type="button" @click="tab='env'" :class="tab==='env' ? '` + classSubTabOn + `' : '` + classSubTabOff + `'" class="` + classSubTab + ` shrink-0">Variables</button>
    <button type="button" @click="tab='deploys'" :class="tab==='deploys' ? '` + classSubTabOn + `' : '` + classSubTabOff + `'" class="` + classSubTab + ` shrink-0">Deployments</button>
    <button type="button" @click="tab='logs'" :class="tab==='logs' ? '` + classSubTabOn + `' : '` + classSubTabOff + `'" class="` + classSubTab + ` shrink-0">Container output</button>
    <button type="button" @click="tab='errors'" :class="tab==='errors' ? '` + classSubTabOn + `' : '` + classSubTabOff + `'" class="` + classSubTab + ` shrink-0">Errors</button>
  </div>

  <div x-show="tab==='overview'" class="space-y-5">
    <p class="` + classLabel + ` ` + classMono + `">
      :{{.Project.Port}} → :{{.Project.ContainerPort}}
      <span class="text-faint">·</span> restarts: <span class="{{if gt .Restarts 0}}text-warn{{else}}text-ink{{end}}">{{.Restarts}}</span>
    </p>

    {{with .Stats}}
    <div class="space-y-1.5">
      {{template "metric-row" (dict "Label" (printf "CPU %.1f%%" .CPUPercent) "Spark" $.CPUSpark)}}
      {{template "metric-row" (dict "Label" (printf "Mem %.0f / %.0f MB" .MemUsedMB .MemLimitMB) "Spark" $.MemSpark)}}
    </div>
    {{end}}

    {{template "uptime-strip" (uptimeCtx .Uptime (printf "/projects/%s/check?view=detail" .Project.Name) "#project-app-content" "outerHTML")}}

    <form hx-post="/projects/{{.Project.Name}}/domain?view=detail" hx-target="#project-app-content" hx-swap="outerHTML" class="flex gap-2">
      <input name="domain" placeholder="example.com" value="{{.Project.Domain}}" class="` + classInput + ` flex-1">
      <button class="` + classBtnSec + `">Set domain</button>
    </form>

    <form hx-post="/projects/{{.Project.Name}}/health-check-path?view=detail" hx-target="#project-app-content" hx-swap="outerHTML" class="flex gap-2">
      <input name="health_check_path" placeholder="/healthz" value="{{.Project.HealthCheckPath}}" class="` + classInput + ` ` + classMono + ` flex-1">
      <button class="` + classBtnSec + `">Set health check path</button>
    </form>
    <p class="text-xs text-faint -mt-3">used both before switching traffic to a new deploy and by the uptime poller above — defaults to "/" (any response counts as healthy); a custom path must return 2xx to count as healthy</p>
  </div>

  <div x-show="tab==='env'" x-cloak class="space-y-5">
    <div class="space-y-1.5">
      <div class="flex items-center justify-between">
        <span class="` + classLabel + `">Shared variables</span>
        <a href="/projects/{{.Project.Name}}" class="text-xs text-faint hover:text-ink transition-colors">edit on the project page →</a>
      </div>
      <pre class="` + classMono + ` ` + classConsole + ` text-cink overflow-x-auto">{{if .Project.CustomEnv}}{{.Project.CustomEnv}}{{else}}(none set){{end}}</pre>
      <p class="text-xs text-faint">plus the database env, if this project has one — both are passed in automatically, on top of the app variables below</p>
    </div>

    <form hx-post="/projects/{{.Project.Name}}/app/env?view=detail" hx-target="#project-app-content" hx-swap="outerHTML" class="space-y-2">
      <div class="flex items-center justify-between">
        <span class="` + classLabel + `">App variables</span>
        <span class="text-xs text-faint">this service only — applies on next redeploy</span>
      </div>
      {{template "env-editor" (dict "Value" .Project.AppEnv "SuggestURL" (printf "/projects/%s/env/suggest" .Project.Name))}}
      <button class="` + classBtnSec + `">Save</button>
    </form>

    {{template "effective-env" .}}
  </div>

  <div x-show="tab==='deploys'" x-cloak class="max-h-96 overflow-y-auto" hx-get="/projects/{{.Project.Name}}/logs" hx-trigger="load"></div>

  <div x-show="tab==='logs'" x-cloak class="max-h-96 overflow-y-auto" hx-get="/projects/{{.Project.Name}}/container-logs" hx-trigger="load"></div>

  <div x-show="tab==='errors'" x-cloak class="max-h-96 overflow-y-auto" hx-get="/projects/{{.Project.Name}}/errors" hx-trigger="load"></div>
</div>
{{end}}

{{define "project-app-page"}}<!doctype html>
<html><head><title>{{.Project.Name}} · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <div class="` + classCard + `">
      {{template "project-app-content" .}}
    </div>
  </div>
</body></html>{{end}}

{{define "project-database-page"}}<!doctype html>
<html><head><title>{{.ProjectName}} · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/projects/{{.ProjectName}}" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← {{.ProjectName}}</a>

    <div class="` + classCard + `">
      {{template "project-database" .}}
    </div>
  </div>
</body></html>{{end}}

{{define "project-storage-page"}}<!doctype html>
<html><head><title>{{.ProjectName}} · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/projects/{{.ProjectName}}" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← {{.ProjectName}}</a>

    <div class="` + classCard + `">
      {{template "project-storage" .}}
    </div>
  </div>
</body></html>{{end}}

{{define "project-storage"}}
<div id="project-storage" class="space-y-4">
  <div class="flex items-center gap-2 min-w-0">
    <span class="w-7 h-7 rounded-md bg-storageaccent/10 text-storageaccent border border-storageaccent/20 flex items-center justify-center shrink-0">` + iconStorageLg + `</span>
    <h2 class="font-medium truncate">Storage</h2>
  </div>

  {{if .LinkedStorage}}
  <p class="` + classLabel + ` ` + classMono + `">linked: {{.LinkedStorage}}</p>
  <p class="text-xs text-faint">injects STORAGE_PROVIDER / S3_ACCESS_KEY_ID / S3_SECRET_ACCESS_KEY / S3_BUCKET_NAME / S3_REGION (+ R2_ACCOUNT_ID or S3_ENDPOINT) on next redeploy</p>
  <button hx-delete="/projects/{{.ProjectName}}/storage" hx-target="#project-storage" hx-swap="outerHTML"
    hx-confirm="Unlink storage {{.LinkedStorage}} from this project?" class="` + classBtnGhost + ` text-xs">
    Unlink
  </button>
  {{else}}
  <form hx-post="/projects/{{.ProjectName}}/storage" hx-target="#project-storage" hx-swap="outerHTML" class="space-y-2">
    {{if .Storages}}
    <select name="storage_name" class="` + classSelect + ` w-full">
      {{range .Storages}}<option value="{{.Name}}">{{.Name}} ({{.Provider}} · {{.Bucket}})</option>{{end}}
    </select>
    <button class="` + classBtnPri + ` w-full">Link storage</button>
    {{else}}
    <p class="text-xs text-faint">No storages registered yet — add one in <a href="/settings/storage" class="text-accent hover:underline">Settings</a> first.</p>
    {{end}}
  </form>
  {{end}}
</div>
{{end}}

{{define "settings-page"}}<!doctype html>
<html><head><title>Settings · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← Dashboard</a>

    <div class="` + classCard + ` space-y-1.5">
      <a href="/settings/database" class="flex items-center gap-2.5 py-1 hover:opacity-70 transition-opacity">
        <span class="w-7 h-7 rounded-md bg-dbaccent/10 text-dbaccent border border-dbaccent/20 flex items-center justify-center shrink-0">` + iconDBLg + `</span>
        <span>
          <span class="block text-sm font-medium">Database</span>
          <span class="block text-xs text-faint">the shared Postgres service every project's database lives in</span>
        </span>
      </a>
      <a href="/settings/storage" class="flex items-center gap-2.5 py-1 hover:opacity-70 transition-opacity">
        <span class="w-7 h-7 rounded-md bg-storageaccent/10 text-storageaccent border border-storageaccent/20 flex items-center justify-center shrink-0">` + iconStorageLg + `</span>
        <span>
          <span class="block text-sm font-medium">Storage</span>
          <span class="block text-xs text-faint">the S3/R2 bucket used for backups and linked projects</span>
        </span>
      </a>
      <a href="/settings/access" class="flex items-center gap-2.5 py-1 hover:opacity-70 transition-opacity">
        <span>
          <span class="block text-sm font-medium">Access</span>
          <span class="block text-xs text-faint">public host + how to expose the panel, projects and databases via a custom domain, Cloudflare or Tailscale</span>
        </span>
      </a>
      <a href="/settings/github" class="flex items-center gap-2.5 py-1 hover:opacity-70 transition-opacity">
        <span>
          <span class="block text-sm font-medium">GitHub</span>
          <span class="block text-xs text-faint">connect a GitHub App for auto-deploy on push</span>
        </span>
      </a>
    </div>
  </div>
</body></html>{{end}}

{{define "settings-access-page"}}<!doctype html>
<html><head><title>Access settings · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}
    <a href="/settings" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← Settings</a>
    {{template "settings-access-content" .}}
  </div>
</body></html>{{end}}

{{define "settings-access-content"}}
<div id="settings-access-content" class="` + classCard + ` space-y-4">
  <div>
    <h2 class="font-medium">Access</h2>
    <p class="text-xs text-faint">Everything listens on localhost only — each project on 127.0.0.1:&lt;port&gt;, the panel itself on 127.0.0.1:9000. Set a domain below and hako gets its own Let's Encrypt cert and routes it automatically (no separate reverse proxy) — or expose things yourself with Cloudflare or Tailscale instead. (Deploys stay zero-downtime either way: an internal proxy swaps traffic between blue/green containers behind that port.)</p>
  </div>
  <form hx-post="/settings/access" hx-target="#settings-access-content" hx-swap="outerHTML" class="space-y-2">
    <label class="block text-xs text-faint">Public host — set this to auto-HTTPS the panel on that domain (also used for SENTRY_DSN + webhook URL){{if .EnvManaged}}. Managed by HAKO_PUBLIC_HOST, form disabled{{end}}</label>
    <input name="public_host" placeholder="panel.example.com" value="{{.PublicHost}}" {{if .EnvManaged}}disabled{{end}} class="` + classInput + ` w-full ` + classMono + `">
    {{if not .EnvManaged}}<button class="` + classBtnPri + `">Save</button>{{end}}
  </form>
  <div class="space-y-1.5 pt-3 border-t border-edge">
    <p class="text-xs font-medium">Panel itself (127.0.0.1:9000) — protected by the token login either way</p>
    <p class="text-xs text-faint">Point DNS for the public host above at this server and it just works — hako gets the cert itself. Or, bring your own edge instead:</p>
    <p class="text-xs text-faint">Cloudflare quick tunnel (public URL, no account):</p>
    <pre class="` + classConsole + ` text-xs overflow-x-auto">cloudflared tunnel --url http://127.0.0.1:9000</pre>
    <p class="text-xs text-faint">Tailscale (private, only your tailnet):</p>
    <pre class="` + classConsole + ` text-xs overflow-x-auto">tailscale serve --bg --https=443 http://127.0.0.1:9000</pre>
  </div>
  {{range .Projects}}
  <div class="space-y-1.5 pt-3 border-t border-edge">
    <p class="text-xs ` + classMono + `">{{.Name}} → 127.0.0.1:{{.Port}}{{if .Domain}} · {{.Domain}} (DNS pointed here → hako auto-HTTPS's it, no extra setup){{end}}</p>
    {{if not .Domain}}<p class="text-xs text-faint">Set a domain on the project's page for automatic HTTPS, or bring your own edge:</p>{{end}}
    <p class="text-xs text-faint">Cloudflare:</p>
    <pre class="` + classConsole + ` text-xs overflow-x-auto">cloudflared tunnel --url http://127.0.0.1:{{.Port}}</pre>
    <p class="text-xs text-faint">Tailscale:</p>
    <pre class="` + classConsole + ` text-xs overflow-x-auto">tailscale serve --bg --https=443 http://127.0.0.1:{{.Port}}</pre>
  </div>
  {{else}}
  <p class="text-xs text-faint">no projects yet — per-project commands will show up here.</p>
  {{end}}
  {{if .Databases}}
  <div class="space-y-1.5 pt-3 border-t border-edge">
    <p class="text-xs font-medium">Databases — never expose publicly; Tailscale only</p>
    {{range .Databases}}
    <p class="text-xs ` + classMono + `">{{.Name}} → 127.0.0.1:{{.Port}}</p>
    {{end}}
    <pre class="` + classConsole + ` text-xs overflow-x-auto">tailscale serve --bg --tcp={{if .Databases}}{{(index .Databases 0).Port}}{{end}} tcp://127.0.0.1:{{if .Databases}}{{(index .Databases 0).Port}}{{end}}</pre>
    <p class="text-xs text-faint">then connect your client to &lt;machine&gt;.tailnet:port. (Cloudflare can do TCP too via cloudflared access, but Tailscale is simpler for databases.)</p>
  </div>
  {{end}}
</div>{{end}}

{{define "settings-github-page"}}<!doctype html>
<html><head><title>GitHub settings · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}
    <a href="/settings" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← Settings</a>
    {{template "settings-github-content" .}}
  </div>
</body></html>{{end}}

{{define "settings-github-content"}}
<div id="settings-github-content" class="` + classCard + ` space-y-3">
  <div>
    <h2 class="font-medium">GitHub</h2>
    {{if .Connected}}
    <p class="text-xs text-good">connected — App ID {{.AppID}}{{if .Slug}} ({{.Slug}}){{end}}. Restart the agent to pick up changes.</p>
    {{else}}
    <p class="text-xs text-faint">not connected — push-to-deploy is off. Create a GitHub App, set its webhook URL to https://&lt;public-host&gt;/webhook/github, install it on your repos, then paste the credentials below. Restart the agent afterwards.</p>
    {{end}}
  </div>
  <form hx-post="/settings/github" hx-target="#settings-github-content" hx-swap="outerHTML" class="space-y-2">
    <input name="app_id" placeholder="App ID" value="{{if .AppID}}{{.AppID}}{{end}}" class="` + classInput + ` w-full ` + classMono + `">
    <input name="slug" placeholder="App slug" value="{{.Slug}}" class="` + classInput + ` w-full ` + classMono + `">
    <textarea name="private_key" placeholder="Private key (PEM)" rows="4" class="` + classInput + ` w-full ` + classMono + `"></textarea>
    <input name="webhook_secret" placeholder="Webhook secret" class="` + classInput + ` w-full ` + classMono + `">
    <input name="client_id" placeholder="Client ID" class="` + classInput + ` w-full ` + classMono + `">
    <input name="client_secret" placeholder="Client secret" class="` + classInput + ` w-full ` + classMono + `">
    <button class="` + classBtnPri + `">Save GitHub App</button>
  </form>
</div>{{end}}

{{define "setup-page"}}<!doctype html>
<html><head><title>Setup · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-2xl mx-auto px-6 py-8">
    {{template "page-header"}}
    <div class="` + classCard + ` space-y-4">
      <div>
        <h2 class="font-medium">First-run setup</h2>
        <p class="text-xs text-faint">No token needed — this page disappears once a project or GitHub App exists. How you got here: SSH tunnel (<code class="` + classMono + `">ssh -L 9000:127.0.0.1:9000 user@host</code>), Tailscale, or local browser. After saving, restart the agent so the GitHub webhook registers, then sign in with the API token from the agent's console.</p>
      </div>
      {{if .Saved}}<p class="text-xs text-good">saved — restart the agent, then continue at /login.</p>{{end}}
      <form method="post" action="/setup" class="space-y-2">
        <label class="block text-xs text-faint">Public host (SENTRY_DSN + webhook URL), e.g. panel.example.com</label>
        <input name="public_host" placeholder="panel.example.com" value="{{.PublicHost}}" class="` + classInput + ` w-full ` + classMono + `">
        <div class="pt-2 space-y-2 border-t border-edge">
          <p class="text-xs text-faint">GitHub App (optional now — also in Settings → GitHub later). Webhook URL: https://&lt;public-host&gt;/webhook/github</p>
          <input name="app_id" placeholder="App ID" class="` + classInput + ` w-full ` + classMono + `">
          <input name="slug" placeholder="App slug" class="` + classInput + ` w-full ` + classMono + `">
          <textarea name="private_key" placeholder="Private key (PEM)" rows="4" class="` + classInput + ` w-full ` + classMono + `"></textarea>
          <input name="webhook_secret" placeholder="Webhook secret" class="` + classInput + ` w-full ` + classMono + `">
          <input name="client_id" placeholder="Client ID" class="` + classInput + ` w-full ` + classMono + `">
          <input name="client_secret" placeholder="Client secret" class="` + classInput + ` w-full ` + classMono + `">
        </div>
        <button class="` + classBtnPri + `">Save and continue</button>
      </form>
    </div>
  </div>
</body></html>{{end}}

{{define "settings-database-page"}}<!doctype html>
<html><head><title>Database settings · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/settings" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← Settings</a>

    {{template "settings-database-content" .}}
  </div>
</body></html>{{end}}

{{define "settings-database-content"}}
<div id="settings-database-content" class="` + classCard + ` space-y-3">
  <div>
    <h2 class="font-medium">Database</h2>
    <p class="text-xs text-faint">One shared Postgres container hosts every project's own database and role — created here, or automatically the first time a project adds a database.</p>
  </div>

  {{if .Postgres}}
  <p class="text-xs text-good">running — version {{.Postgres.Version}}</p>
  {{else}}
  <form hx-post="/settings/database" hx-target="#settings-database-content" hx-swap="outerHTML" class="space-y-2">
    <select name="version" class="` + classSelect + ` w-full">
      {{range .Versions}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
    <button class="` + classBtnPri + `">Start Postgres service</button>
  </form>
  <p class="text-xs text-faint">only asked once — every project's database shares this one version from then on</p>
  {{end}}

  <div class="space-y-1.5 pt-3 border-t border-edge">
    {{range .Databases}}
    <div class="flex items-center justify-between gap-3 ` + classConsole + `">
      <p class="text-xs ` + classMono + `">{{.Name}}{{if .BackupStorage}} <span class="text-faint">· backs up to {{.BackupStorage}}</span>{{end}}</p>
      <button hx-delete="/settings/databases/{{.Name}}" hx-target="#settings-database-content" hx-swap="outerHTML"
        hx-confirm="Delete database {{.Name}}? Data will be permanently lost." class="` + classBtnGhost + ` shrink-0 text-xs">
        Delete
      </button>
    </div>
    {{else}}
    <p class="text-xs text-faint">no databases created yet</p>
    {{end}}
  </div>

  <details class="text-sm">
    <summary class="cursor-pointer text-xs text-dim hover:text-ink transition-colors">+ Add database</summary>
    <form hx-post="/settings/databases" hx-target="#settings-database-content" hx-swap="outerHTML" class="space-y-2 mt-2">
      <input name="name" placeholder="database name" required class="` + classInput + ` w-full">
      {{if not .Postgres}}
      <select name="version" class="` + classSelect + ` w-full">
        {{range .Versions}}<option value="{{.}}">{{.}}</option>{{end}}
      </select>
      {{end}}
      <button class="` + classBtnPri + `">Add database</button>
    </form>
  </details>
</div>
{{end}}

{{define "settings-storage-page"}}<!doctype html>
<html><head><title>Storage settings · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/settings" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← Settings</a>

    {{template "settings-storage-content" .}}
  </div>
</body></html>{{end}}

{{define "settings-storage-content"}}
<div id="settings-storage-content" class="` + classCard + ` space-y-3">
  <div>
    <h2 class="font-medium">Storage</h2>
    <p class="text-xs text-faint">Register as many buckets as you actually have — one is usually enough. Link one to a project on that project's own Storage page, or pick one for a database's backups on that database's Backups panel.</p>
  </div>

  <div class="space-y-1.5">
    {{range .Storages}}
    <div class="flex items-center justify-between gap-3 ` + classConsole + `">
      <p class="text-xs ` + classMono + `">{{.Name}} <span class="text-faint">· {{.Provider}} · {{.Bucket}}</span></p>
      <button hx-delete="/settings/storages/{{.Name}}" hx-target="#settings-storage-content" hx-swap="outerHTML"
        hx-confirm="Delete storage {{.Name}}? Refuses if a project or backups still use it." class="` + classBtnGhost + ` shrink-0 text-xs">
        Delete
      </button>
    </div>
    {{else}}
    <p class="text-xs text-faint">no storages registered yet</p>
    {{end}}
  </div>

  <details class="text-sm">
    <summary class="cursor-pointer text-xs text-dim hover:text-ink transition-colors">+ Add storage</summary>
    <form hx-post="/settings/storages" hx-target="#settings-storage-content" hx-swap="outerHTML" class="space-y-2 mt-2" x-data="{provider:'r2'}">
      <input name="name" placeholder="name (e.g. r2)" required class="` + classInput + ` w-full ` + classMono + `">
      <select name="provider" x-model="provider" class="` + classSelect + ` w-full">
        <option value="r2">Cloudflare R2</option>
        <option value="s3">S3-compatible (external)</option>
        <option value="rustfs">Self-hosted (RustFS)</option>
      </select>
      <div x-show="provider!=='rustfs'" class="space-y-2">
        <input name="account_id" placeholder="Cloudflare account ID" x-show="provider==='r2'" class="` + classInput + ` w-full ` + classMono + `">
        <input name="endpoint" placeholder="Endpoint URL" x-show="provider==='s3'" x-cloak class="` + classInput + ` w-full ` + classMono + `">
        <input name="access_key_id" placeholder="Access key ID" :required="provider!=='rustfs'" class="` + classInput + ` w-full ` + classMono + `">
        <input name="secret_access_key" type="password" placeholder="Secret access key" :required="provider!=='rustfs'" class="` + classInput + ` w-full ` + classMono + `">
        <input name="bucket" placeholder="Bucket name" :required="provider!=='rustfs'" class="` + classInput + ` w-full ` + classMono + `">
        <input name="region" placeholder="Region (default: auto)" class="` + classInput + ` w-full ` + classMono + `">
      </div>
      <p x-show="provider==='rustfs'" x-cloak class="text-xs text-faint">Hako runs its own RustFS container and generates its own keys — nothing to type in.</p>
      <button class="` + classBtnPri + `" x-text="provider==='rustfs' ? 'Set up RustFS' : 'Add storage'"></button>
    </form>
  </details>
  <p class="text-xs text-faint">an R2 storage named "r2-env" can also be synced from R2_ACCOUNT_ID / R2_ACCESS_KEY_ID / R2_SECRET_ACCESS_KEY / R2_BUCKET_NAME in the agent's environment instead — either way ends up here</p>
</div>
{{end}}

{{define "metric-row"}}
<div class="flex items-center justify-between text-xs text-dim">
  <span class="` + classMono + `">{{.Label}}</span>
  <span>{{.Spark}}</span>
</div>
{{end}}

{{define "uptime-strip"}}
<div class="flex items-center justify-between gap-3 text-xs text-dim">
  <div class="flex items-center gap-3 flex-wrap ` + classMono + `">
    {{if .Uptime.HasData}}
    <span>Uptime <span class="text-ink">{{printf "%.2f" .Uptime.UptimePercent}}%</span></span>
    <span>Response <span class="text-ink">{{.Uptime.LastResponseMs}} ms</span></span>
    <span class="text-faint">Checked <span class="text-dim">{{.Uptime.CheckedAgo}}</span></span>
    {{else}}
    <span>no checks yet</span>
    {{end}}
  </div>
  <div class="flex items-center gap-2.5 shrink-0">
    <div class="flex items-end gap-0.5">
      {{range .Uptime.Bars}}<span class="w-1 h-4 rounded-[1px] {{if .}}bg-good{{else}}bg-bad{{end}}"></span>{{end}}
    </div>
    <button type="button" hx-post="{{.CheckURL}}" hx-target="{{.Target}}" hx-swap="{{.Swap}}"
      class="text-dim hover:text-ink transition-colors" title="Check now">↻</button>
  </div>
</div>
{{end}}

{{define "deploy-logs"}}
<div class="space-y-2">
  {{range .Logs}}
  <div class="` + classConsole + `">
    <p class="text-xs text-cdim ` + classMono + `">{{.CreatedAt}} · {{.Trigger}} ·
      <span class="{{if eq .Status "success"}}text-good{{else}}text-bad{{end}}">{{.Status}}</span>
    </p>
    <pre class="text-xs whitespace-pre-wrap mt-1.5 text-cink ` + classMono + `">{{.Output}}</pre>
  </div>
  {{else}}
  <p class="text-xs text-faint">no deploys logged yet</p>
  {{end}}
</div>
{{end}}

{{define "database-backups"}}
<div id="database-backups" class="space-y-2">
  <div class="flex items-center justify-between">
    <span class="` + classLabel + `">Backups</span>
    {{if .BackupStorage}}
    <button hx-post="/projects/{{.ProjectName}}/database/backups" hx-target="#database-backups" hx-swap="outerHTML" class="` + classBtnGhost + `">
      Backup now
    </button>
    {{end}}
  </div>

  {{if .Storages}}
  <form hx-post="/projects/{{.ProjectName}}/database/backups/storage" hx-target="#database-backups" hx-swap="outerHTML" class="flex gap-2 items-center">
    <span class="text-xs text-faint shrink-0">Upload to</span>
    <select name="storage_name" class="` + classSelect + ` flex-1 text-xs">
      <option value="">— pick a storage —</option>
      {{range .Storages}}<option value="{{.Name}}" {{if eq .Name $.BackupStorage}}selected{{end}}>{{.Name}} ({{.Provider}} · {{.Bucket}})</option>{{end}}
    </select>
    <button class="` + classBtnGhost + ` text-xs">Save</button>
  </form>
  {{else}}
  <p class="text-xs text-faint">no storages registered yet — add one in <a href="/settings/storage" class="text-accent hover:underline">Settings</a> to enable backups</p>
  {{end}}

  {{if not .BackupStorage}}
  <p class="text-xs text-faint">pick a storage above to enable backups for this database</p>
  {{else}}
  <div class="space-y-1.5">
    {{range .Backups}}
    <div class="flex items-center justify-between gap-3 ` + classConsole + `">
      <div class="min-w-0">
        <p class="text-xs text-cdim ` + classMono + `">{{.CreatedAt}}</p>
        <p class="text-xs text-faint ` + classMono + ` truncate" title="{{.ObjectKey}}">{{.ObjectKey}} · {{printf "%.1f" (divf .SizeBytes 1048576.0)}} MB</p>
      </div>
      <button hx-post="/projects/{{$.ProjectName}}/database/backups/restore" hx-target="#database-backups" hx-swap="outerHTML"
        hx-vals='{"object_key": "{{.ObjectKey}}"}'
        hx-confirm="Restore {{$.DBName}} from this backup? Conflicting existing rows may cause errors rather than being overwritten."
        class="` + classBtnGhost + ` shrink-0 text-xs">
        Restore
      </button>
    </div>
    {{else}}
    <p class="text-xs text-faint">no backups yet</p>
    {{end}}
  </div>
  {{end}}
</div>
{{end}}

{{define "telemetry-events"}}
<div class="space-y-2">
  {{range .Events}}
  <div class="` + classConsole + `">
    <p class="text-xs text-cdim ` + classMono + `">{{.CreatedAt}} ·
      <span class="{{if eq .Kind "error"}}text-bad{{else}}text-dim{{end}}">{{.Kind}}</span>
      {{if .Level}}· {{.Level}}{{end}}
    </p>
    <pre class="text-xs whitespace-pre-wrap mt-1.5 text-cink ` + classMono + `">{{.Message}}</pre>
  </div>
  {{else}}
  <p class="text-xs text-faint">no errors or logs received yet — point this project's SENTRY_DSN env var at Hako (auto-injected once a public host is set in Settings → Access) and it'll show up here</p>
  {{end}}
</div>
{{end}}

{{define "container-logs"}}
<pre class="text-xs whitespace-pre-wrap ` + classMono + ` text-cink ` + classConsole + ` max-h-96 overflow-y-auto">{{if .}}{{.}}{{else}}(no output yet){{end}}</pre>
{{end}}

{{define "project-database"}}
<div id="project-database" class="space-y-4">
  {{if .HasDB}}
  {{with .DB}}
  {{$db := .}}
  <div class="flex items-center justify-between gap-3">
    <div class="flex items-center gap-2 min-w-0">
      <span class="w-7 h-7 rounded-md bg-dbaccent/10 text-dbaccent border border-dbaccent/20 flex items-center justify-center shrink-0">` + iconDBLg + `</span>
      <h2 class="font-medium truncate">{{.Database.Name}}</h2>
      <span class="inline-flex items-center gap-1.5 text-xs shrink-0 {{if .Ready}}text-good{{else}}text-bad{{end}}">
        <span class="w-1.5 h-1.5 rounded-full {{if .Ready}}bg-good{{else}}bg-bad{{end}}"></span>
        {{if .Ready}}accepting connections{{else}}not responding{{end}}
      </span>
    </div>
    <button hx-get="/projects/{{$.ProjectName}}/database" hx-target="#project-database" hx-swap="outerHTML" class="` + classBtnGhost + `" aria-label="Refresh">↻</button>
  </div>

  <div class="space-y-3">
    <div class="flex justify-end">
      <button hx-delete="/projects/{{$.ProjectName}}/database" hx-target="#project-database" hx-swap="outerHTML"
        hx-confirm="Unlink database {{.Database.Name}} from this project? The database itself keeps running — delete it in Settings if you want it gone for good."
        class="` + classBtnGhost + ` text-xs">
        Unlink
      </button>
    </div>

    <p class="` + classLabel + ` ` + classMono + `">
      {{.Database.Kind}} · :{{.Database.Port}}
      <span class="text-faint">·</span> disk {{printf "%.0f" .DiskUsedMB}} MB
      <span class="text-faint">·</span> connections {{.Connections}}
    </p>

    {{with .Stats}}
    <div class="space-y-1.5">
      {{template "metric-row" (dict "Label" (printf "CPU %.1f%%" .CPUPercent) "Spark" $db.CPUSpark)}}
      {{template "metric-row" (dict "Label" (printf "Mem %.0f / %.0f MB" .MemUsedMB .MemLimitMB) "Spark" $db.MemSpark)}}
    </div>
    {{end}}

    {{template "uptime-strip" (uptimeCtx .Uptime (printf "/projects/%s/database/check" $.ProjectName) "#project-database" "outerHTML")}}

    <div x-data="{show:false}">
      <button type="button" @click="show = !show" class="` + classBtnGhost + `">
        <span x-text="show ? 'Hide env' : 'Show env'"></span>
      </button>
      <pre x-show="show" class="` + classMono + ` ` + classConsole + ` mt-2 overflow-x-auto text-cink">POSTGRES_USER={{.Database.DBUser}}
POSTGRES_PASSWORD={{.Database.DBPassword}}
POSTGRES_DB={{.Database.DBName}}
POSTGRES_HOST={{.Database.ContainerName}}
POSTGRES_PORT={{.Database.Port}}</pre>
    </div>

    <div id="database-backups" hx-get="/projects/{{$.ProjectName}}/database/backups" hx-trigger="load" hx-swap="outerHTML">
      <span class="text-xs text-faint">Loading backups…</span>
    </div>
  </div>
  {{end}}
  {{else}}
  <div class="flex items-center gap-2 min-w-0">
    <span class="w-7 h-7 rounded-md bg-dbaccent/10 text-dbaccent border border-dbaccent/20 flex items-center justify-center shrink-0">` + iconDBLg + `</span>
    <h2 class="font-medium truncate">Database</h2>
  </div>
  <form hx-post="/projects/{{.ProjectName}}/database" hx-target="#project-database" hx-swap="outerHTML" class="space-y-2">
    {{if .Databases}}
    <select name="db_name" class="` + classSelect + ` w-full">
      {{range .Databases}}<option value="{{.Name}}">{{.Name}}</option>{{end}}
    </select>
    <button class="` + classBtnPri + ` w-full">Link database</button>
    {{else}}
    <p class="text-xs text-faint">No databases created yet — add one in <a href="/settings/database" class="text-accent hover:underline">Settings</a> first.</p>
    {{end}}
  </form>
  {{end}}
</div>
{{end}}

{{define "project-worker"}}
<div id="project-worker" class="space-y-4">
  {{if .HasWorker}}
  {{with .Worker}}
  {{$w := .}}
  <div class="flex items-center justify-between gap-3">
    <div class="flex items-center gap-2 min-w-0">
      <span class="w-7 h-7 rounded-md bg-workeraccent/10 text-workeraccent border border-workeraccent/20 flex items-center justify-center shrink-0">` + iconWorkerLg + `</span>
      <h2 class="font-medium truncate">{{.Worker.Name}}</h2>
      <span class="inline-flex items-center gap-1.5 text-xs shrink-0 {{if .Up}}text-good{{else}}text-bad{{end}}">
        <span class="w-1.5 h-1.5 rounded-full {{if .Up}}bg-good{{else}}bg-bad{{end}}"></span>
        {{if .Up}}running{{else}}not running{{end}}
      </span>
    </div>
    <div class="flex items-center gap-2 shrink-0">
      <button hx-post="/projects/{{$.ProjectName}}/worker/redeploy" hx-target="#project-worker" hx-swap="outerHTML" class="` + classBtnSec + `">
        Redeploy
      </button>
      <button hx-delete="/projects/{{$.ProjectName}}/worker" hx-target="#project-worker" hx-swap="outerHTML"
        hx-confirm="Delete worker {{.Worker.Name}}? Its container will be stopped and removed."
        class="` + classBtnDanger + ` text-xs">
        Delete
      </button>
    </div>
  </div>

  <div class="space-y-3">
    <p class="` + classLabel + ` ` + classMono + `">
      restarts: <span class="{{if gt .Restarts 0}}text-warn{{else}}text-ink{{end}}">{{.Restarts}}</span>
    </p>
    <pre class="` + classMono + ` ` + classConsole + ` text-cink overflow-x-auto">{{.Worker.Command}}</pre>

    {{with .Stats}}
    <div class="space-y-1.5">
      {{template "metric-row" (dict "Label" (printf "CPU %.1f%%" .CPUPercent) "Spark" $w.CPUSpark)}}
      {{template "metric-row" (dict "Label" (printf "Mem %.0f / %.0f MB" .MemUsedMB .MemLimitMB) "Spark" $w.MemSpark)}}
    </div>
    {{end}}

    {{template "uptime-strip" (uptimeCtx .Uptime (printf "/projects/%s/worker/check" $.ProjectName) "#project-worker" "outerHTML")}}

    <div class="space-y-1.5">
      <div class="flex items-center justify-between">
        <span class="` + classLabel + `">Shared variables</span>
        <a href="/projects/{{$.ProjectName}}" class="text-xs text-faint hover:text-ink transition-colors">edit on the project page →</a>
      </div>
      <pre class="` + classMono + ` ` + classConsole + ` text-cink overflow-x-auto">{{if $.SharedEnv}}{{$.SharedEnv}}{{else}}(none set){{end}}</pre>
      <p class="text-xs text-faint">plus the database env, if this project has one — both are passed in automatically, on top of the worker variables below</p>
    </div>

    <form hx-post="/projects/{{$.ProjectName}}/worker/env" hx-target="#project-worker" hx-swap="outerHTML" class="space-y-2">
      <div class="flex items-center justify-between">
        <span class="` + classLabel + `">Worker variables</span>
        <span class="text-xs text-faint">this service only — applies on next redeploy</span>
      </div>
      {{template "env-editor" (dict "Value" .Worker.CustomEnv "SuggestURL" (printf "/projects/%s/env/suggest" $.ProjectName))}}
      <button class="` + classBtnSec + `">Save</button>
    </form>

    <div x-data="{show:false}">
      <button type="button" @click="show = !show" hx-get="/projects/{{$.ProjectName}}/worker/container-logs" hx-target="#worker-logs" hx-trigger="click once" class="` + classBtnGhost + `">
        <span x-text="show ? 'Hide container output' : 'Show container output'"></span>
      </button>
      <div id="worker-logs" x-show="show" class="mt-2"></div>
    </div>
  </div>
  {{end}}
  {{else}}
  <div class="flex items-center gap-2 min-w-0">
    <span class="w-7 h-7 rounded-md bg-workeraccent/10 text-workeraccent border border-workeraccent/20 flex items-center justify-center shrink-0">` + iconWorkerLg + `</span>
    <h2 class="font-medium truncate">Worker</h2>
  </div>
  <form hx-post="/projects/{{.ProjectName}}/worker" hx-target="#project-worker" hx-swap="outerHTML" class="space-y-2">
    <p class="text-xs text-faint">No worker yet — runs the same image as the app, with its own start command and no exposed port.</p>
    <input name="name" placeholder="worker name" required class="` + classInput + ` w-full">
    <input name="command" placeholder="start command, e.g. procrastinate --app=myapp.app worker" required class="` + classInput + ` w-full ` + classMono + `">
    <button class="` + classBtnPri + ` w-full">Create worker</button>
  </form>
  {{end}}
</div>
{{end}}

{{define "project-worker-page"}}<!doctype html>
<html><head><title>{{.ProjectName}} · Hako</title>` + pageHead + `</head>
<body class="bg-canvas text-ink min-h-screen antialiased">
  <div class="max-w-4xl mx-auto px-6 py-8">
    {{template "page-header"}}

    <a href="/projects/{{.ProjectName}}" class="inline-flex items-center gap-1 text-xs text-faint hover:text-ink transition-colors mb-4">← {{.ProjectName}}</a>

    <div class="` + classCard + `">
      {{template "project-worker" .}}
    </div>
  </div>
</body></html>{{end}}

{{define "new-project-form"}}
<form hx-post="/projects/scan" hx-target="#new-project-modal-body" hx-swap="innerHTML" class="space-y-3">
  <input name="name" placeholder="project name" required autofocus class="` + classInput + ` w-full">
  <input name="repo" placeholder="owner/repo" required class="` + classInput + ` w-full">
  <input name="domain" placeholder="domain (optional)" class="` + classInput + ` w-full">
  <input name="container_port" type="number" placeholder="container port" value="8080" class="` + classInput + ` w-full">
  <button class="` + classBtnPri + ` w-full">Scan repo</button>
</form>
{{end}}

{{define "preset-picker"}}
<form hx-post="/projects" hx-target="#new-project-modal-body" hx-swap="innerHTML" class="space-y-3">
  <p class="text-sm text-dim">Pick a build preset for <span class="` + classMono + ` text-ink">{{.Repo}}</span></p>
  <div class="space-y-2">
    {{range $i, $p := .Presets}}
    <label class="flex items-center gap-2 text-sm text-dim">
      <input type="radio" name="preset" value="{{$p.Path}}::{{$p.Strategy}}" {{if eq $i 0}}checked{{end}} class="accent-accent">
      {{$p.Label}} <span class="text-faint ` + classMono + `">— {{$p.Path}}</span>
    </label>
    {{end}}
  </div>
  <input type="hidden" name="name" value="{{.Name}}">
  <input type="hidden" name="repo" value="{{.Repo}}">
  <input type="hidden" name="domain" value="{{.Domain}}">
  <input type="hidden" name="container_port" value="{{.ContainerPort}}">
  <button class="` + classBtnPri + ` w-full">Create project</button>
</form>
{{end}}

{{define "projects-partial"}}
<div class="space-y-2">
  {{range .Cards}}{{template "project-row" .}}{{else}}
    <p class="text-sm text-faint">No projects yet. Click "New project" to deploy your first one.</p>
  {{end}}
</div>
{{end}}

`

var templates = template.Must(template.New("root").Funcs(template.FuncMap{
	"statusColor": statusColor,
	"uptimeCtx":   uptimeCtx,
	"dict":        dict,
	"divf":        func(a int64, b float64) float64 { return float64(a) / b },
}).Parse(templatesSrc))

// dict lets a template call pass a small ad-hoc struct-like value inline —
// used to feed the shared "metric-row" partial, which otherwise would need
// a dedicated Go type per call site for no real benefit.
func dict(pairs ...interface{}) map[string]interface{} {
	m := make(map[string]interface{}, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1]
	}
	return m
}

type uptimeStripCtx struct {
	Uptime   uptimeView
	CheckURL string
	Target   string
	Swap     string
}

func uptimeCtx(u uptimeView, checkURL, target, swap string) uptimeStripCtx {
	return uptimeStripCtx{Uptime: u, CheckURL: checkURL, Target: target, Swap: swap}
}

type projectsView struct {
	Cards []projectCardView
}

type projectCardView struct {
	Project    store.Project
	Status     string
	Responding string
	Restarts   int
	Uptime     uptimeView
	Stats      *deploy.ContainerStats
	CPUSpark   template.HTML
	MemSpark   template.HTML
	HasDB      bool
	DB         databaseCardView
	HasWorker  bool
	Worker     workerCardView
	Versions   []string
	// EffectiveEnv is what the app container actually runs with on its next
	// deploy: linked-database vars + shared vars + the auto-injected
	// SENTRY_DSN + the app's own vars — the "Variables" tab shows this
	// alongside the editable CustomEnv/AppEnv boxes so the DB connection
	// and DSN (which nothing lets you type in directly) are still visible.
	EffectiveEnv []envVar
}

// envVar is one KEY=VALUE pair from ops.AppEnv, split for display — parsed
// once in Go rather than in the template so a value that itself contains
// "=" (e.g. a DSN's query string) doesn't get mangled by a naive split.
type envVar struct {
	Key   string
	Value string
}

func splitEnvVars(env []string) []envVar {
	vars := make([]envVar, 0, len(env))
	for _, line := range env {
		key, value, _ := strings.Cut(line, "=")
		vars = append(vars, envVar{Key: key, Value: value})
	}
	return vars
}

type projectDatabaseView struct {
	ProjectName string
	HasDB       bool
	DB          databaseCardView
	Databases   []store.Database
}

type projectStorageView struct {
	ProjectName   string
	LinkedStorage string
	Storages      []store.Storage
}

type databaseSettingsView struct {
	Postgres  *store.PostgresService
	Versions  []string
	Databases []store.Database
}

type storageSettingsView struct {
	Storages []store.Storage
}

type accessProjectView struct {
	Name   string
	Port   int
	Domain string
}

type accessDBView struct {
	Name string
	Port int
}

type accessView struct {
	PublicHost string
	EnvManaged bool
	Projects   []accessProjectView
	Databases  []accessDBView
}

func buildAccessView(s *store.Store) accessView {
	v := accessView{PublicHost: config.PublicBaseURL()}
	if os.Getenv("HAKO_PUBLIC_HOST") != "" {
		v.EnvManaged = true
	}
	if projects, err := s.ListProjects(); err == nil {
		for _, p := range projects {
			v.Projects = append(v.Projects, accessProjectView{Name: p.Name, Port: p.Port, Domain: p.Domain})
		}
	}
	if dbs, err := s.ListDatabases(); err == nil {
		for _, d := range dbs {
			v.Databases = append(v.Databases, accessDBView{Name: d.Name, Port: d.Port})
		}
	}
	return v
}

type githubView struct {
	Connected bool
	Slug      string
	AppID     int64
}

func buildGitHubView(s *store.Store) githubView {
	app, err := s.GetGitHubApp()
	if err != nil {
		return githubView{}
	}
	return githubView{Connected: true, Slug: app.Slug, AppID: app.AppID}
}

func parseAppID(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

type databaseBackupsView struct {
	ProjectName   string
	DBName        string
	Backups       []store.Backup
	Storages      []store.Storage
	BackupStorage string
}

type databaseCardView struct {
	Database    store.Database
	Ready       bool
	Uptime      uptimeView
	Stats       *deploy.ContainerStats
	CPUSpark    template.HTML
	MemSpark    template.HTML
	DiskUsedMB  float64
	Connections int
}

type workerView struct {
	ProjectName string
	HasWorker   bool
	Worker      workerCardView
	// SharedEnv is the project's own custom env, shown read-only here — a
	// worker always gets it automatically (see ops.workerEnv), on top of
	// its own worker-only variables.
	SharedEnv string
}

type workerCardView struct {
	Worker   store.Worker
	Up       bool
	Restarts int
	Uptime   uptimeView
	Stats    *deploy.ContainerStats
	CPUSpark template.HTML
	MemSpark template.HTML
}

// hostView is the home-page "Server" card — the machine's own CPU/memory
// trend (sampled every 10s by the health poller into the same
// resource_samples table projects/databases use) plus a live disk/load
// snapshot, so the dashboard leads with whether the host itself is healthy
// before it lists what's running on it.
type hostView struct {
	HasData                 bool
	CPUPercent              float64
	MemUsedGB, MemTotalGB   float64
	DiskUsedGB, DiskTotalGB float64
	Load1, Load5, Load15    float64
	CPUSpark, MemSpark      template.HTML
}

func buildHostView(s *store.Store) hostView {
	h, err := hostinfo.Get("data")
	if err != nil {
		return hostView{}
	}
	cpuSpark, memSpark := buildResourceSparklines(s, "host")
	const mbPerGB = 1024.0
	return hostView{
		HasData:     true,
		CPUPercent:  h.CPUPercent,
		MemUsedGB:   h.MemUsedMB / mbPerGB,
		MemTotalGB:  h.MemTotalMB / mbPerGB,
		DiskUsedGB:  h.DiskUsedGB,
		DiskTotalGB: h.DiskTotalGB,
		Load1:       h.Load1,
		Load5:       h.Load5,
		Load15:      h.Load15,
		CPUSpark:    cpuSpark,
		MemSpark:    memSpark,
	}
}

// statsFor fetches CPU/memory only for running containers — stats calls are
// relatively expensive and meaningless for a stopped/restarting container.
func statsFor(ctx context.Context, containerName, status string) *deploy.ContainerStats {
	if status != "running" {
		return nil
	}
	stats, err := deploy.GetContainerStats(ctx, containerName)
	if err != nil {
		return nil
	}
	return &stats
}

// Muted line — a resource sparkline like this is read as a texture/shape
// (busy vs. quiet), not a value to color-match against anything else, so a
// de-emphasis gray reads better than a bold hue here.
const sparkColor = "#a1a1aa"

const sparklinePoints = 60

// sparklineSVG renders a minimal trend line (dataviz skill: 2px, round
// joins, no axes/gridlines — a stat-tile sparkline, not a full chart).
func sparklineSVG(values []float64, colorHex string) template.HTML {
	if len(values) < 2 {
		return template.HTML(`<span class="text-faint text-xs">no data yet</span>`)
	}

	const w, h, pad = 140.0, 28.0, 2.0
	min, max := values[0], values[0]
	for _, v := range values {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	rng := max - min
	if rng == 0 {
		rng = 1
	}

	step := (w - 2*pad) / float64(len(values)-1)
	var pts strings.Builder
	for i, v := range values {
		x := pad + step*float64(i)
		y := pad + (h-2*pad)*(1-(v-min)/rng)
		if i > 0 {
			pts.WriteString(" ")
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
	}

	svg := fmt.Sprintf(
		`<svg viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" class="inline-block align-middle" role="img" aria-label="trend"><polyline points="%s" fill="none" stroke="%s" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/></svg>`,
		w, h, w, h, pts.String(), colorHex,
	)
	return template.HTML(svg)
}

func buildResourceSparklines(s *store.Store, target string) (cpu, mem template.HTML) {
	samples, _ := s.ListResourceSamples(target, sparklinePoints)
	cpuValues := make([]float64, len(samples))
	memValues := make([]float64, len(samples))
	for i, sm := range samples {
		cpuValues[i] = sm.CPUPercent
		memValues[i] = sm.MemUsedMB
	}
	return sparklineSVG(cpuValues, sparkColor), sparklineSVG(memValues, sparkColor)
}

// uptimeView renders as the classic uptime-monitor strip: a row of
// green/red bars (oldest to newest), the percentage up over that window,
// the most recent response time, and how long ago the last check ran.
type uptimeView struct {
	Bars           []bool
	UptimePercent  float64
	LastResponseMs int
	CheckedAgo     string
	HasData        bool
}

const uptimeBarCount = 40

func buildUptime(checks []store.HealthCheck) uptimeView {
	if len(checks) == 0 {
		return uptimeView{}
	}

	bars := make([]bool, 0, len(checks))
	upCount := 0
	for _, c := range checks {
		bars = append(bars, c.Up)
		if c.Up {
			upCount++
		}
	}

	last := checks[len(checks)-1]
	return uptimeView{
		Bars:           bars,
		UptimePercent:  float64(upCount) / float64(len(checks)) * 100,
		LastResponseMs: last.ResponseMs,
		CheckedAgo:     humanizeAgo(last.CheckedAt),
		HasData:        true,
	}
}

func humanizeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		mins := int(d.Minutes())
		return fmt.Sprintf("%d minute(s) ago", mins)
	default:
		hours := int(d.Hours())
		return fmt.Sprintf("%d hour(s) ago", hours)
	}
}

type deployLogsView struct {
	Project string
	Logs    []store.DeployLog
}

type telemetryEventsView struct {
	Project string
	Events  []store.TelemetryEvent
}

type presetPickerView struct {
	Name          string
	Repo          string
	Domain        string
	ContainerPort int
	Presets       []detect.Preset
}

// buildProjectCard adds the container's live docker status plus its uptime
// history (from the background health poller) so the dashboard shows
// whether the site is actually up over time, not just that the container
// process exists (a crash-looping app still shows as a docker container,
// just restarting endlessly).
func buildProjectCard(ctx context.Context, s *store.Store, project store.Project, withResources bool) projectCardView {
	status, restarts, err := deploy.ContainerStatus(ctx, project.ContainerName())
	if err != nil {
		status = "unknown"
	}

	checks, _ := s.ListHealthChecks("project:"+project.Name, uptimeBarCount)
	uptime := buildUptime(checks)

	responding := ""
	if status == "running" {
		if uptime.HasData {
			if uptime.Bars[len(uptime.Bars)-1] {
				responding = "responding"
			} else {
				responding = "not responding"
			}
		} else {
			// No history yet (just created) — check once so the card
			// isn't blank until the next poller tick.
			up, _ := ops.CheckProjectHealth(s, project)
			if up {
				responding = "responding"
			} else {
				responding = "not responding"
			}
			checks, _ = s.ListHealthChecks("project:"+project.Name, uptimeBarCount)
			uptime = buildUptime(checks)
		}
	}

	stats := statsFor(ctx, project.ContainerName(), status)
	cpuSpark, memSpark := buildResourceSparklines(s, "project:"+project.Name)

	effectiveEnv, _ := ops.AppEnv(s, project)

	card := projectCardView{Project: project, Status: status, Responding: responding, Restarts: restarts, Uptime: uptime, Stats: stats, CPUSpark: cpuSpark, MemSpark: memSpark, Versions: config.PostgresVersions, EffectiveEnv: splitEnvVars(effectiveEnv)}
	if withResources {
		if project.LinkedDB != "" {
			if dbInfo, err := s.GetDatabase(project.LinkedDB); err == nil {
				card.HasDB = true
				card.DB = buildDatabaseCards(ctx, s, []store.Database{*dbInfo})[0]
			}
		}
		if worker, err := s.GetWorker(project.Name); err == nil {
			card.HasWorker = true
			card.Worker = buildWorkerCard(ctx, s, *worker)
		}
	}
	return card
}

// buildDatabaseCards adds each database's uptime history (pg_isready over
// time) instead of just a single point-in-time check.
func buildDatabaseCards(ctx context.Context, s *store.Store, databases []store.Database) []databaseCardView {
	cards := make([]databaseCardView, 0, len(databases))
	for _, d := range databases {
		checks, _ := s.ListHealthChecks("database:"+d.Name, uptimeBarCount)
		uptime := buildUptime(checks)

		ready := uptime.HasData && uptime.Bars[len(uptime.Bars)-1]
		if !uptime.HasData {
			ready = ops.CheckDatabaseHealth(s, d)
			checks, _ = s.ListHealthChecks("database:"+d.Name, uptimeBarCount)
			uptime = buildUptime(checks)
		}

		// d.ContainerName is now Hako's one shared Postgres service, not
		// a container of this database's own — CPU/mem here reflect the whole
		// service (same number on every project's card), so DiskMB/Connections
		// below (queried per logical database) are the numbers actually
		// specific to this project.
		status, _, _ := deploy.ContainerStatus(ctx, d.ContainerName)
		stats := statsFor(ctx, d.ContainerName, status)
		cpuSpark, memSpark := buildResourceSparklines(s, "database:"+d.Name)

		var diskMB float64
		var connections int
		if status == "running" {
			diskMB, _ = deploy.PostgresDatabaseSizeMB(ctx, d.ContainerName, d.DBName)
			connections, _ = deploy.PostgresConnectionCount(ctx, d.ContainerName, d.DBName)
		}

		cards = append(cards, databaseCardView{
			Database: d, Ready: ready, Uptime: uptime, Stats: stats,
			CPUSpark: cpuSpark, MemSpark: memSpark,
			DiskUsedMB: diskMB, Connections: connections,
		})
	}
	return cards
}

// buildWorkerCard adds the worker container's live docker status plus its
// uptime history — "up" here just means the container is running (see
// ops.CheckWorkerHealth), there's no HTTP endpoint to probe like a project
// or database has.
func buildWorkerCard(ctx context.Context, s *store.Store, worker store.Worker) workerCardView {
	checks, _ := s.ListHealthChecks("worker:"+worker.ProjectName, uptimeBarCount)
	uptime := buildUptime(checks)

	up := uptime.HasData && uptime.Bars[len(uptime.Bars)-1]
	if !uptime.HasData {
		up = ops.CheckWorkerHealth(s, worker.ProjectName)
		checks, _ = s.ListHealthChecks("worker:"+worker.ProjectName, uptimeBarCount)
		uptime = buildUptime(checks)
	}

	status, restarts, _ := deploy.ContainerStatus(ctx, worker.ContainerName())
	stats := statsFor(ctx, worker.ContainerName(), status)
	cpuSpark, memSpark := buildResourceSparklines(s, "worker:"+worker.ProjectName)

	return workerCardView{
		Worker: worker, Up: up, Restarts: restarts, Uptime: uptime, Stats: stats,
		CPUSpark: cpuSpark, MemSpark: memSpark,
	}
}

func statusColor(status string) string {
	switch status {
	case "running":
		return "bg-good"
	case "restarting":
		return "bg-warn"
	default:
		return "bg-bad"
	}
}
