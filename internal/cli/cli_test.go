package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type machine struct{ *nodetest.Fake }

func (machine) Logs(context.Context, string, int) (string, error)              { return "", nil }
func (machine) CheckDump(context.Context, node.DBSpec, io.Reader) (int, error) { return 0, nil }
func (machine) LoadDump(context.Context, node.DBSpec, io.Reader) error         { return nil }
func (machine) ArchiveVolume(context.Context, string, string, io.Writer) error { return nil }
func (machine) CheckArchive(io.Reader) (int, error)                            { return 0, nil }
func (machine) LoadArchive(context.Context, node.AppSpec, string, io.Reader, io.Writer) error {
	return nil
}

// run runs the CLI with stdin, returning what it printed.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	oldOut, oldIn := os.Stdout, os.Stdin
	r, w, _ := os.Pipe()
	in, inW, _ := os.Pipe()
	os.Stdout, os.Stdin = w, in
	go func() { _, _ = inW.WriteString(stdin); inW.Close() }()
	cmd := Root("test")
	cmd.SetArgs(args)
	err := cmd.Execute()
	w.Close()
	os.Stdout, os.Stdin = oldOut, oldIn
	var b bytes.Buffer
	_, _ = io.Copy(&b, r)
	return b.String(), err
}

func TestLaptop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	owner, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	cfg, err := server.ParseConfig([]byte(fmt.Sprintf(`
name = "kasl-1"
listen = "100.64.0.1:7070"
data_dir = %q
owner_key = %q
operator_token_sha256 = %q
admin_token_sha256 = %q
releases = ["ghcr.io/kasl/crm-release:main"]
`, dir, owner.Recipient().String(), server.HashToken("op"), server.HashToken("admin"))))
	if err != nil {
		t.Fatal(err)
	}
	serverKey, _ := server.InitKey(cfg)
	d, err := server.New(cfg, machine{nodetest.New()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.API())
	defer srv.Close()

	if _, err := run(t, "wrong\n", "server", "add", "kasl-1", srv.URL); err == nil {
		t.Error("added with a wrong token")
	}
	if _, err := run(t, "op\n", "server", "add", "kasl-2", srv.URL); err == nil || !strings.Contains(err.Error(), "that's server kasl-1") {
		t.Errorf("added under another name: %v", err)
	}
	out, err := run(t, "op\n", "server", "add", "kasl-1", srv.URL)
	if err != nil || !strings.Contains(out, serverKey) {
		t.Fatalf("add: %q, %v", out, err)
	}
	file := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hakobu", "servers.toml")
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("servers.toml: %v %v", st, err)
	}

	out, err = run(t, "", "status")
	if err != nil || !strings.Contains(out, "kasl-1") || !strings.Contains(out, "backups: NONE") {
		t.Errorf("status: %q, %v", out, err)
	}

	// The agent's view: the same servers, over MCP, without restore or
	// destroy.
	root := Root("test")
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
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, " "); strings.Contains(got, "restore") || strings.Contains(got, "destroy") || !strings.Contains(got, "rollback") {
		t.Errorf("tools: %s", got)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil || res.IsError {
		t.Fatalf("status: %+v, %v", res, err)
	}
	if b, _ := json.Marshal(res.StructuredContent); !strings.Contains(string(b), `"server":"kasl-1"`) {
		t.Errorf("status over MCP: %s", b)
	}

	// A project's secrets, in its repository.
	repo := t.TempDir()
	t.Chdir(repo)
	if err := os.WriteFile("hakobu.toml", []byte("project = \"kasl\"\nserver = \"kasl-1\"\n[apps.crm]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "", "secret", "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "sk_live_1\n", "secret", "set", "crm", "STRIPE_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "github_pat\n", "secret", "set", "hakobu", seal.StatusToken); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(seal.FileName)
	if strings.Contains(string(b), "sk_live_1") || !strings.Contains(string(b), serverKey) || !strings.Contains(string(b), owner.Recipient().String()) {
		t.Errorf("hakobu.secrets.toml:\n%s", b)
	}
	s, _ := seal.Parse(b)
	id, _ := seal.LoadIdentity(cfg.KeyFile())
	if o, err := s.Open(id, "kasl"); err != nil || o.Apps["crm"]["STRIPE_KEY"] != "sk_live_1" || o.Hakobu[seal.StatusToken] != "github_pat" {
		t.Errorf("the server opens %+v, %v", o, err)
	}
	if out, _ := run(t, "", "secret", "list"); out != "crm\tSTRIPE_KEY\nhakobu\tstatus_token\n" {
		t.Errorf("list %q", out)
	}
	if out, err := run(t, "", "validate"); err != nil || out != "ok\n" {
		t.Errorf("validate: %q %v", out, err)
	}
	if err := os.WriteFile("hakobu.toml", []byte("project = \"kasl\"\nserver = \"kasl-1\"\n[apps.crm]\nhelth = \"/\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "", "validate"); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Errorf("validate a typo: %v", err)
	}

	// Destroying asks a person, and a pipe isn't one.
	if _, err := run(t, "kasl/main\n", "destroy", "kasl/main"); err == nil || !strings.Contains(err.Error(), "a person at a terminal") {
		t.Errorf("destroy from a pipe: %v", err)
	}
}
