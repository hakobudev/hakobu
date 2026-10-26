package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hakobudev/hakobu/internal/seal"
)

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{
		"":                         {},
		"  status ":                {"status"},
		"logs kasl/web --lines 50": {"logs", "kasl/web", "--lines", "50"},
		`server set mail kasl --from "Kasl <a@b.c>"`: {"server", "set", "mail", "kasl", "--from", "Kasl <a@b.c>"},
		`a 'b c' d\ e ""`:  {"a", "b c", "d e", ""},
		"status; rm -rf /": {"status;", "rm", "-rf", "/"},
		"status $(id)":     {"status", "$(id)"},
	} {
		got, err := splitWords(in)
		if err != nil || len(got)+len(want) > 0 && !slices.Equal(got, want) {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	if _, err := splitWords(`status "x`); err == nil {
		t.Error("took an open quote")
	}
}

func TestSSHAllowed(t *testing.T) {
	for _, ok := range []string{"mcp", "status", "logs kasl/web", "secret set kasl web X", "server set mail kasl", "server update", "--version"} {
		w, _ := splitWords(ok)
		if !sshAllowed(w) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, no := range []string{"bash", "serve", "restore kasl/main", "destroy kasl/main", "server init", "server token", "server create x", "secret init",
		"build --image x", "ssh", "dialer /tmp/x", "--server x status", "statuses", "server", "secret unset kasl web X", "init", "scp -t /"} {
		w, _ := splitWords(no)
		if sshAllowed(w) {
			t.Errorf("allowed %q", no)
		}
		if err := runSSH(context.Background(), "v1.5.0", no, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "isn't one of them") {
			t.Errorf("%q: %v", no, err)
		}
	}
}

// A fake server on a socket, as hakobu ssh finds it.
func fakeSocket(t *testing.T, h http.Handler) {
	dir, err := os.MkdirTemp("", "hk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no servers.toml
	old := localSocket
	localSocket = sock
	t.Cleanup(func() { localSocket = old })
}

func TestSSHSecretSet(t *testing.T) {
	server, _ := age.GenerateX25519Identity()
	owner, _ := age.GenerateX25519Identity()
	var got map[string]string
	fakeSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/key":
			_ = json.NewEncoder(w).Encode(map[string]string{"server": "kasl-1", "recipient": server.Recipient().String(), "owner": owner.Recipient().String()})
		case "/v1/secrets":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	stdin, w, _ := os.Pipe()
	_, _ = w.WriteString("s3cret\n")
	w.Close()
	old := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = old })

	var out bytes.Buffer
	if err := runSSH(context.Background(), "v1.5.0", "secret set kasl web DATABASE_URL", &out); err != nil {
		t.Fatal(err)
	}
	if got["project"] != "kasl" || got["app"] != "web" || got["name"] != "DATABASE_URL" || strings.Contains(fmt.Sprint(got), "s3cret") {
		t.Errorf("sent %v", got)
	}
	for _, id := range []age.Identity{server, owner} {
		if v, err := seal.OpenValue(got["sealed"], id); err != nil || v != "s3cret" {
			t.Errorf("opens to %q, %v", v, err)
		}
	}
	if !strings.Contains(out.String(), "kasl-1") {
		t.Errorf("output: %s", out.String())
	}
	if err := runSSH(context.Background(), "v1.5.0", "server set backups kasl --bucket b", &out); err == nil || !strings.Contains(err.Error(), "root's") {
		t.Errorf("buckets: %v", err)
	}
	if err := runSSH(context.Background(), "v1.5.0", "secret set kasl web", &out); err == nil {
		t.Error("took secret set without a name")
	}
	out.Reset()
	if err := runSSH(context.Background(), "v1.5.0", "", &out); err != nil || !strings.Contains(out.String(), "hakobu's commands on kasl-1") {
		t.Errorf("help: %v %s", err, out.String())
	}
}

// The agent's tools on a server (ssh … mcp): the server is the socket's.
func TestMCPOnServer(t *testing.T) {
	server, _ := age.GenerateX25519Identity()
	owner, _ := age.GenerateX25519Identity()
	var settings map[string]any
	fakeSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/key":
			_ = json.NewEncoder(w).Encode(map[string]string{"server": "kasl-1", "recipient": server.Recipient().String(), "owner": owner.Recipient().String()})
		case "/v1/settings":
			_ = json.NewDecoder(r.Body).Decode(&settings)
			_, _ = w.Write([]byte(`{"rev":1}`))
		case "/v1/secrets":
			_, _ = w.Write([]byte(`[{"project":"kasl","app":"web","name":"X","sealed":"age:abc"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("SSH_CONNECTION", "203.0.113.9 50000 2.29.109.71 22")
	root := Root("v1.5.0")
	mcpCmd, _, _ := root.Find([]string{"mcp"})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := newMCP(mcpCmd).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := user.Current(); !strings.Contains(session.InitializeResult().Instructions, "ssh -t "+u.Username+"@2.29.109.71 secret set PROJECT APP NAME") {
		t.Errorf("instructions don't name the address:\n%s", session.InitializeResult().Instructions)
	}
	call := func(name string, args map[string]any) (map[string]any, string) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			return nil, fmt.Sprint(res.Content[0].(*mcp.TextContent).Text)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, ""
	}

	out, msg := call("init", map[string]any{"project": "kasl", "image": "ghcr.io/x0ryz/kasl-crm"})
	if msg != "" {
		t.Fatal(msg)
	}
	files, _ := out["files"].(map[string]any)
	for _, f := range []string{"hakobu.toml", "hakobu.secrets.toml", ".github/workflows/hakobu.yml", ".claude/skills/hakobu/SKILL.md", "CLAUDE.md", ".claude/settings.json"} {
		if files[f] == nil {
			t.Errorf("init gave no %s", f)
		}
	}
	if s, _ := files["hakobu.toml"].(string); !strings.Contains(s, `project = "kasl"`) || !strings.Contains(s, `server = "kasl-1"`) {
		t.Errorf("hakobu.toml:\n%s", s)
	}
	if s, _ := files[".github/workflows/hakobu.yml"].(string); !strings.Contains(s, "build.yml@v1.5.0") {
		t.Errorf("workflow:\n%s", s)
	}
	if settings["add_release"] != "ghcr.io/x0ryz/kasl-crm:main" {
		t.Errorf("registered %v", settings)
	}

	spec, _ := files["hakobu.toml"].(string)
	secrets, _ := files["hakobu.secrets.toml"].(string)
	if _, msg := call("validate", map[string]any{"hakobu_toml": spec, "hakobu_secrets_toml": secrets}); msg != "" {
		t.Errorf("validate init's: %s", msg)
	}
	if _, msg := call("validate", map[string]any{"hakobu_toml": spec + "\nnope = 1\n"}); !strings.Contains(msg, "nope") {
		t.Errorf("validate a bad one: %q", msg)
	}
	other, _ := seal.New(owner.Recipient().String())
	b, _ := other.Marshal()
	if _, msg := call("validate", map[string]any{"hakobu_toml": spec, "hakobu_secrets_toml": string(b)}); !strings.Contains(msg, "isn't sealed to kasl-1") {
		t.Errorf("validate secrets the server can't open: %q", msg)
	}

	out, msg = call("pending_secrets", map[string]any{"project": "kasl"})
	if ps, _ := out["secrets"].([]any); msg != "" || len(ps) != 1 {
		t.Errorf("pending: %v %s", out, msg)
	}
}

// On a server, its name may be left out.
func TestNameFirst(t *testing.T) {
	fakeSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"server":"kasl-1"}`))
	}))
	for _, c := range []struct {
		in   string
		want string
	}{
		{"", "kasl-1"},
		{"kasl-1", "kasl-1"},
		{"ghcr.io/kasl/crm:main", "kasl-1 ghcr.io/kasl/crm:main"},
		{"kasl-1 ghcr.io/kasl/crm:main", "kasl-1 ghcr.io/kasl/crm:main"},
	} {
		in, _ := splitWords(c.in)
		got, err := nameFirst(in, 1, 2)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%q: %q %v", c.in, got, err)
		}
	}
	if _, err := nameFirst([]string{"kasl-1"}, 2, 2); err == nil {
		t.Error("took release with no image")
	}
}
