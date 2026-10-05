package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
