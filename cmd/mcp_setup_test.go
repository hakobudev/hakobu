package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// An AI app allowed to deploy sets apps up for its user, in their own
// projects, servers and Cloudflare accounts only.
func TestMCPSetup(t *testing.T) {
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
	me, err := s.CreateUser(ctx, store.CreateUserParams{GitHubID: 42, GitHubLogin: "me", Admin: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, store.CreateUserParams{GitHubID: 7, GitHubLogin: "friend"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		project, app, db string
		user             int64
	}{{"mine", "web", "", me}, {"other", "theirs", "theirdb", other}} {
		if err := s.CreateUserProject(ctx, store.CreateUserProjectParams{Name: a.project, UserID: a.user}); err != nil {
			t.Fatal(err)
		}
		p, _ := s.GetProject(ctx, a.project)
		if err := s.CreateApp(ctx, store.CreateAppParams{ProjectID: p.ID, Name: a.app, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if a.db != "" {
			if err := s.CreateDatabase(ctx, store.CreateDatabaseParams{ProjectID: p.ID, Name: a.db, User: a.db + "_user", Password: "x"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.CreateNode(ctx, store.CreateNodeParams{Name: "theirsrv", UserID: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCloudflareAccount(ctx, store.CreateCloudflareAccountParams{Name: "theiracct", ApiToken: "t", AccountID: "a", UserID: other}); err != nil {
		t.Fatal(err)
	}

	token := func(scope string) string {
		grant, err := s.CreateOAuthGrant(ctx, store.CreateOAuthGrantParams{ClientID: "c", ClientName: "VS Code", RedirectURI: "http://127.0.0.1/", Scope: scope, GitHubID: 42})
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := s.NewOAuthToken(ctx, grant, "access", "", store.OAuthAccessTTL)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	h := panelHandler(mux)
	call := func(tok, tool, args string) string {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		r := httptest.NewRequest("POST", "https://hakobu.example.com/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %d, %v", tool, w.Code, err)
		}
		return jsonString(out)
	}
	deploy, read := token("read deploy"), token("read")

	// What someone else owns doesn't exist for the tools.
	for _, c := range []struct{ tool, args, want string }{
		{"create_project", `{"name":"new","server":"theirsrv"}`, `no server named \"theirsrv\"`},
		{"create_project", `{"name":"new","cloudflare_account":"theiracct"}`, `no Cloudflare account named \"theiracct\"`},
		{"create_app", `{"project":"other","name":"x","repo":"o/r"}`, `no project named \"other\"`},
		{"create_database", `{"project":"other","name":"d"}`, `no project named \"other\"`},
		{"link_database", `{"app":"web","database":"theirdb"}`, `no database named \"theirdb\"`},
		{"set_env", `{"app":"theirs","set":{"A":"1"}}`, `no app named \"theirs\"`},
		{"add_volume", `{"app":"theirs","name":"data","mount_path":"/data"}`, `no app named \"theirs\"`},
	} {
		if out := call(deploy, c.tool, c.args); !strings.Contains(out, c.want) {
			t.Errorf("%s %s: %s, want %s", c.tool, c.args, out, c.want)
		}
	}
	if _, err := s.GetProject(ctx, "new"); err == nil {
		t.Error("a project was made on someone else's server or account")
	}

	// A read-only connection changes nothing.
	if out := call(read, "set_env", `{"app":"web","set":{"A":"1"}}`); !strings.Contains(out, "may only read") {
		t.Errorf("set_env with a read-only token: %s", out)
	}

	// Its own: projects listed, variables set, a volume added.
	if out := call(deploy, "list_projects", `{}`); !strings.Contains(out, `\"name\":\"mine\"`) || strings.Contains(out, "other") || strings.Contains(out, "theirsrv") {
		t.Errorf("list_projects: %s", out)
	}
	if out := call(deploy, "set_env", `{"app":"web","set":{"DATABASE_PATH":"/data/app.db"}}`); !strings.Contains(out, "set 1") {
		t.Errorf("set_env: %s", out)
	}
	if out := call(deploy, "add_volume", `{"app":"web","name":"data","mount_path":"/data"}`); !strings.Contains(out, "added volume data") {
		t.Errorf("add_volume: %s", out)
	}
	if app, _ := s.GetApp(ctx, "web"); string(app.Env) != "DATABASE_PATH=/data/app.db" {
		t.Errorf("web's variables: %q", app.Env)
	}
	if vols, _ := s.ListVolumes(ctx, "web"); len(vols) != 1 || vols[0].MountPath != "/data" {
		t.Errorf("web's volumes: %v", vols)
	}
	if out := call(deploy, "create_project", `{"name":"shop"}`); !strings.Contains(out, "created project shop") {
		t.Errorf("create_project: %s", out)
	}
	if p, err := s.GetProject(ctx, "shop"); err != nil || p.UserID != me {
		t.Errorf("shop isn't the caller's: %+v %v", p, err)
	}
	if out := call(deploy, "get_app", `{"app":"web"}`); !strings.Contains(out, "https://hakobu.example.com/apps/web/variables") {
		t.Errorf("get_app has no panel_url: %s", out)
	}
}
