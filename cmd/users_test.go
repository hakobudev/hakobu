package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/store"
)

// fakeGitHubUsers signs in the GitHub account named by the OAuth code.
func fakeGitHubUsers(t *testing.T, users map[string]int64) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + r.FormValue("code")})
		case "/user":
			login := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": users[login], "login": login})
		case "/user/emails":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldWeb := github.APIURL, github.WebURL
	github.APIURL, github.WebURL = srv.URL, srv.URL
	t.Cleanup(func() { github.APIURL, github.WebURL = oldAPI, oldWeb })
}

// The installer's setup link makes the first user the admin; anyone else
// gets in only with an invite, which works once; a removed user is out at
// once; only the admin reaches the panel's own settings.
func TestUsersSignIn(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("hakobu.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := config.SetSetupToken("setup"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SaveGitHubApp(ctx, store.SaveGitHubAppParams{AppID: 1, Slug: "hakobu", PrivateKey: "k", WebhookSecret: "w", ClientID: "c", ClientSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	fakeGitHubUsers(t, map[string]int64{"me": 42, "friend": 7, "stranger": 9})
	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	do := func(method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://hakobu.example.com"+path, nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		return w
	}
	cookie := func(name, value string) *http.Cookie { return &http.Cookie{Name: name, Value: value} }
	state := cookie(oauthStateCookie, "st")
	signIn := func(login string, cookies ...*http.Cookie) (session string, w *httptest.ResponseRecorder) {
		w = do("GET", "/auth/callback?state=st&code="+login, append(cookies, state)...)
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie && c.Value != "" {
				session = c.Value
			}
		}
		return session, w
	}
	users := func() int64 {
		n, _ := s.CountUsers(ctx)
		return n
	}

	if sess, w := signIn("stranger"); sess != "" || !strings.HasPrefix(w.Header().Get("Location"), "/login?error=") || users() != 0 {
		t.Fatalf("a stranger claimed the panel: %d %v", w.Code, w.Header())
	}
	admin, _ := signIn("me", cookie(setupCookie, "setup"))
	if a, _ := s.Admin(ctx); admin == "" || a.GitHubLogin != "me" || config.SetupToken() != "" {
		t.Fatalf("the setup link didn't make the admin: %+v", a)
	}
	if sess, _ := signIn("stranger", cookie(setupCookie, "setup")); sess != "" {
		t.Error("the setup link let a second account in")
	}

	if err := s.NewInvite(ctx, "inv", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := do("GET", "/invite/inv")
	if w.Header().Get("Location") != "/auth/login" || !strings.Contains(w.Header().Get("Set-Cookie"), inviteCookie+"=inv") {
		t.Errorf("invite link: %d %v", w.Code, w.Header())
	}
	friend, _ := signIn("friend", cookie(inviteCookie, "inv"))
	if u, err := s.GetUserByGitHubID(ctx, 7); friend == "" || err != nil || u.Admin != 0 {
		t.Fatalf("the invite didn't make a user: %+v, %v", u, err)
	}
	if sess, _ := signIn("stranger", cookie(inviteCookie, "inv")); sess != "" {
		t.Error("an invite worked twice")
	}
	if sess, _ := signIn("friend"); sess == "" {
		t.Error("a user couldn't sign in again")
	}

	// The panel's own settings are the admin's.
	if w := do("GET", "/settings/master-key", cookie(sessionCookie, friend)); w.Code != http.StatusNotFound {
		t.Errorf("a user reached the master key: %d", w.Code)
	}
	if w := do("POST", "/settings/update/check", cookie(sessionCookie, friend)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "admin") {
		t.Errorf("a user checked for updates: %d %q", w.Code, w.Body)
	}
	if w := do("POST", "/settings/invites", cookie(sessionCookie, friend)); w.Code != http.StatusBadRequest {
		t.Errorf("a user made an invite: %d", w.Code)
	}
	if w := do("POST", "/settings/invites", cookie(sessionCookie, admin)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "https://hakobu.example.com/invite/") {
		t.Errorf("the admin's invite: %d %q", w.Code, w.Body)
	}
	if w := do("GET", "/settings", cookie(sessionCookie, friend)); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `id="users"`) || strings.Contains(w.Body.String(), `id="update"`) {
		t.Errorf("a user's settings show the admin's parts: %d", w.Code)
	}

	if w := do("DELETE", "/settings/users/1", cookie(sessionCookie, admin)); w.Code != http.StatusBadRequest {
		t.Errorf("the admin removed themselves: %d", w.Code)
	}
	friendUser, _ := s.GetUserByGitHubID(ctx, 7)
	if w := do("DELETE", "/settings/users/"+itoa(friendUser.ID), cookie(sessionCookie, admin)); w.Code != http.StatusOK {
		t.Fatalf("removing a user: %d %q", w.Code, w.Body)
	}
	if w := do("GET", "/settings", cookie(sessionCookie, friend)); w.Code != http.StatusSeeOther {
		t.Errorf("a removed user's session still works: %d", w.Code)
	}
}

// A user reaches nothing of someone else's: every route that names a
// project, app, database, storage, server, Cloudflare account or sealed
// variables answers 404 for another user's, and so do their charts. The
// routes are read from web.go, so a new one is checked too.
func TestUsersSeeOnlyTheirOwn(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("hakobu.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	admin, err := s.CreateUser(ctx, store.CreateUserParams{GitHubID: 42, GitHubLogin: "me", Admin: 1})
	must(err)
	_, err = s.CreateUser(ctx, store.CreateUserParams{GitHubID: 7, GitHubLogin: "friend"})
	must(err)
	must(s.NewSession(ctx, "friend", 7, time.Hour))
	must(s.CreateUserProject(ctx, store.CreateUserProjectParams{Name: "adminp", UserID: admin}))
	p, _ := s.GetProject(ctx, "adminp")
	must(s.CreateApp(ctx, store.CreateAppParams{ProjectID: p.ID, Name: "adminapp", BuildStrategy: "dockerfile"}))
	must(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "admindb", ProjectID: p.ID, User: "u", Password: "x"}))
	must(s.CreateStorage(ctx, store.CreateStorageParams{Name: "adminst", ProjectID: p.ID, Provider: "r2", SecretAccessKey: "x"}))
	must(s.CreateNode(ctx, store.CreateNodeParams{Name: "adminsrv", UserID: admin}))
	_, err = s.CreateCloudflareAccount(ctx, store.CreateCloudflareAccountParams{Name: "adminclient", ApiToken: "x", AccountID: "acc", UserID: admin})
	must(err)

	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	src, err := os.ReadFile(filepath.Join(sourceDir(t), "web.go"))
	must(err)
	names := map[string]string{
		"{p}": "adminp", "{a}": "adminapp", "{d}": "admindb", "{st}": "adminst", "{name}": "adminsrv", "{c}": "adminclient",
		"{scope}": "app", "{owner}": "adminapp", "{key}": "K", "{v}": "data", "{id}": "1", "{trace}": "t",
	}
	checked := 0
	for _, m := range regexp.MustCompile(`(?:handle|action)\("(GET|POST|DELETE) ([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		method, path := m[1], m[2]
		if !regexp.MustCompile(`\{(p|a|d|st|name|c|owner)\}`).MatchString(path) {
			continue
		}
		for k, v := range names {
			path = strings.ReplaceAll(path, k, v)
		}
		r := httptest.NewRequest(method, "https://hakobu.example.com"+path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "friend"})
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s by another user: %d %q", method, path, w.Code, w.Body)
		}
		checked++
	}
	if checked < 40 {
		t.Errorf("only %d routes checked: is the pattern still right?", checked)
	}
	for target, want := range map[string]int{"app:adminapp": http.StatusNotFound, "worker:adminapp": http.StatusNotFound, "host:adminsrv": http.StatusNotFound, "service:postgres:adminsrv": http.StatusNotFound, "panel": http.StatusNotFound, "host": http.StatusOK, "service:postgres": http.StatusOK} {
		for _, path := range []string{"/usage", "/uptime"} {
			if path == "/uptime" && (strings.HasPrefix(target, "host") || strings.HasPrefix(target, "service")) {
				continue
			}
			r := httptest.NewRequest("GET", "https://hakobu.example.com"+path+"?target="+target, nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "friend"})
			w := httptest.NewRecorder()
			panelHandler(mux).ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("%s?target=%s: %d, want %d", path, target, w.Code, want)
			}
		}
	}

	// A panel-only panel's server is the admin's alone.
	config.PanelOnly = true
	for _, target := range []string{"host", "service:postgres"} {
		r := httptest.NewRequest("GET", "https://hakobu.example.com/usage?target="+target, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "friend"})
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("panel-only, /usage?target=%s: %d, want 404", target, w.Code)
		}
	}
	config.PanelOnly = false

	// Lists show only the user's own.
	for _, path := range []string{"/", "/switch/projects", "/settings"} {
		r := httptest.NewRequest("GET", "https://hakobu.example.com"+path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "friend"})
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		for _, name := range []string{"adminp", "adminsrv", "adminclient"} {
			if strings.Contains(w.Body.String(), name) {
				t.Errorf("%s shows %s to another user", path, name)
			}
		}
	}
}

// sourceDir is this package's directory, for tests that read its source.
func sourceDir(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Dir(file)
}
