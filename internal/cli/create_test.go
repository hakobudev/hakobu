package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/hakobudev/hakobu/internal/client"
)

// The test binary stands in for ssh: HAKOBU_FAKE_SSH is a directory where
// it says what it was asked, and FAKE_API is where -W connects.
func TestMain(m *testing.M) {
	if dir := os.Getenv("HAKOBU_FAKE_SSH"); dir != "" {
		os.Exit(fakeSSH(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSH(dir string, args []string) int {
	if i := slices.Index(args, "-W"); i >= 0 {
		_ = os.WriteFile(filepath.Join(dir, "dial"), []byte(strings.Join(args, " ")), 0o600)
		c, err := net.Dial("tcp", os.Getenv("FAKE_API"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect to host: Connection refused")
			return 255
		}
		go func() { _, _ = io.Copy(c, os.Stdin) }()
		_, _ = io.Copy(os.Stdout, c)
		return 0
	}
	_ = os.WriteFile(filepath.Join(dir, "install-args"), []byte(strings.Join(args, "\x00")), 0o600)
	tr := tar.NewReader(os.Stdin)
	var script []byte
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		if h.Name == "install.sh" {
			script = b
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "install.sh"), script, 0o600)
	get := func(k string) string {
		m := regexp.MustCompile(`(?m)^export ` + k + `='([^']*)'$`).FindSubmatch(script)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	fmt.Println("==> packages")
	res := installResult{Name: get("HAKOBU_NAME"), API: "127.0.0.1:7070", Mode: get("HAKOBU_API"), Version: "v1.3.0"}
	// Like install.sh: the name is the config's, made the first time.
	if name, err := os.ReadFile(filepath.Join(dir, "installed")); err == nil {
		res.Name = string(name)
	} else {
		res.OperatorToken, res.AdminToken = "hk_op", "hk_admin"
		_ = os.WriteFile(filepath.Join(dir, "installed"), []byte(res.Name), 0o600)
	}
	b, _ := json.Marshal(res)
	fmt.Printf("HAKOBU_RESULT %s\n", b)
	return 0
}

func TestServerCreate(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("HAKOBU_FAKE_SSH", dir)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hk_op" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"server": "kasl-2", "recipient": "age1server"})
	}))
	defer api.Close()
	t.Setenv("FAKE_API", api.Listener.Addr().String())
	client.SSHCommand = os.Args[0]
	t.Cleanup(func() { client.SSHCommand = "ssh" })
	b, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	InstallScript = string(b)
	t.Cleanup(func() { InstallScript = "" })

	id, _ := age.GenerateX25519Identity()
	o := createOpts{ssh: "root@1.2.3.4", ingress: "public", email: "me@example.com", ownerKey: id.Recipient().String()}
	var out bytes.Buffer
	if err := createServer(context.Background(), "kasl-2", o, "v1.3.0", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	script, _ := os.ReadFile(filepath.Join(dir, "install.sh"))
	for _, want := range []string{
		"export HAKOBU_NAME='kasl-2'\n", "export HAKOBU_API='loopback'\n", "export HAKOBU_INGRESS='public'\n",
		"export HAKOBU_OWNER_KEY='" + id.Recipient().String() + "'\n", "export HAKOBU_VERSION='v1.3.0'\n",
		"export HAKOBU_RESULT='json'\n", "export HAKOBU_TUNNEL_KEY='ssh-ed25519 ", InstallScript,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "install-args"))
	if strings.Contains(string(args), id.Recipient().String()) || !strings.Contains(string(args), "root@1.2.3.4") {
		t.Errorf("ssh's arguments: %q", args)
	}
	if !strings.Contains(out.String(), "hk_admin") || !strings.Contains(out.String(), "  ==> packages") {
		t.Errorf("output:\n%s", out.String())
	}

	all, err := client.LoadServers(filepath.Join(cfg, "hakobu", "servers.toml"))
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(cfg, "hakobu", "keys", "kasl-2")
	want := client.Server{URL: "http://127.0.0.1:7070", Token: "hk_op", SSH: "hakobu-tunnel@1.2.3.4", SSHKey: key}
	if all["kasl-2"] != want {
		t.Errorf("servers.toml has %+v, want %+v", all["kasl-2"], want)
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the key: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(cfg, "hakobu", "owner.pub")); strings.TrimSpace(string(b)) != id.Recipient().String() {
		t.Errorf("owner.pub: %q", b)
	}
	dial, _ := os.ReadFile(filepath.Join(dir, "dial"))
	if !strings.Contains(string(dial), "-i "+key) || !strings.Contains(string(dial), "-W 127.0.0.1:7070 hakobu-tunnel@1.2.3.4") {
		t.Errorf("the API's ssh: %s", dial)
	}

	// Again: an update, whose installer shows no tokens; the laptop keeps
	// the one it has, and the key.
	pub1 := tunnelPub(t, script)
	out.Reset()
	if err := createServer(context.Background(), "kasl-2", o, "v1.3.0", &out); err != nil {
		t.Fatalf("again: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "admin token") {
		t.Errorf("again shows an admin token:\n%s", out.String())
	}
	script, _ = os.ReadFile(filepath.Join(dir, "install.sh"))
	if tunnelPub(t, script) != pub1 {
		t.Error("again made another key")
	}
	all, _ = client.LoadServers(filepath.Join(cfg, "hakobu", "servers.toml"))
	if all["kasl-2"].Token != "hk_op" {
		t.Errorf("again: %+v", all["kasl-2"])
	}

	// Another name for a server that has one is refused, and not kept.
	if err := createServer(context.Background(), "kasl-3", o, "v1.3.0", &out); err == nil || !strings.Contains(err.Error(), "kasl-2") {
		t.Errorf("another name: %v", err)
	}
}

func tunnelPub(t *testing.T, script []byte) string {
	m := regexp.MustCompile(`(?m)^export HAKOBU_TUNNEL_KEY='([^']*)'$`).FindSubmatch(script)
	if m == nil {
		t.Fatal("no HAKOBU_TUNNEL_KEY")
	}
	return string(m[1])
}

// An API that ssh can't reach says what ssh said.
func TestSSHDialSaysWhy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAKOBU_FAKE_SSH", dir)
	t.Setenv("FAKE_API", "127.0.0.1:1")
	client.SSHCommand = os.Args[0]
	t.Cleanup(func() { client.SSHCommand = "ssh" })
	all := client.Servers{"x": {URL: "http://127.0.0.1:7070", Token: "t", SSH: "hakobu-tunnel@nowhere", SSHKey: "/k"}}
	c, err := all.Client("x")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "/v1/status", nil)
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("err = %v", err)
	}
}
