package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/update"
)

// hakobu server create installs hakobu on a server from the laptop, once,
// over the person's own SSH: this release's install.sh goes in on ssh's
// stdin, its settings with it (never in arguments, which ps shows), and
// what it made (the tokens, the API's address) comes back as one line.
// After that the laptop reaches the API with a key that may do nothing
// else, and no one needs a shell on the server.

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// apiWait is how long a new server's API has to answer.
var apiWait = 90 * time.Second

type createOpts struct {
	ssh, ingress, email, tags, ownerKey, version, binary string
	tailscale                                            bool
}

// installResult is install.sh's HAKOBU_RESULT line.
type installResult struct {
	Name          string `json:"name"`
	API           string `json:"api"`
	Mode          string `json:"mode"`
	Key           string `json:"key"`
	Version       string `json:"version"`
	OperatorToken string `json:"operator_token"`
	AdminToken    string `json:"admin_token"`
}

func serverCreateCmd(version string) *cobra.Command {
	var o createOpts
	c := &cobra.Command{
		Use:   "create NAME --ssh root@HOST",
		Short: "Install hakobu on a server over SSH, once, and remember it: after that the laptop reaches its API through a key that may do nothing else",
		Long: `Install hakobu on a server (Debian or Ubuntu) over SSH, as root or a user with sudo, and remember it here.

The API listens on the server's loopback; this laptop reaches it through
SSH with a key made for it (~/.config/hakobu/keys/NAME), which the server
lets reach the API and nothing else. With --tailscale it listens on the
tailnet instead (TS_AUTHKEY from the environment, or asked).

Your age public key comes from --owner-key or ~/.config/hakobu/owner.pub,
or is asked for and kept there. A tunnel's token (--ingress tunnel) comes
from CLOUDFLARE_TUNNEL_TOKEN, or is asked.

Run again on the same server to update it: the tokens stay.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createServer(cmd.Context(), args[0], o, version, cmd.OutOrStdout())
		},
	}
	f := c.Flags()
	f.StringVar(&o.ssh, "ssh", "", "how to reach the server: user@host, or a host of ~/.ssh/config")
	f.StringVar(&o.ingress, "ingress", "none", "how the web gets in: public (80/443, Let's Encrypt), tunnel (Cloudflare Tunnel) or none")
	f.StringVar(&o.email, "email", "", "Let's Encrypt's contact, for --ingress public")
	f.BoolVar(&o.tailscale, "tailscale", false, "the API on the tailnet instead of loopback")
	f.StringVar(&o.tags, "tags", "", "the server's Tailscale tags, with --tailscale (tag:hakobu)")
	f.StringVar(&o.ownerKey, "owner-key", "", "your age public key (default ~/.config/hakobu/owner.pub)")
	f.StringVar(&o.version, "version", "", "the hakobu release to install (default this CLI's, or the latest)")
	f.StringVar(&o.binary, "binary", "", "install this local build of hakobu for linux instead of a release")
	_ = c.MarkFlagRequired("ssh")
	return c
}

func createServer(ctx context.Context, name string, o createOpts, version string, out io.Writer) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q: a server's name is lowercase letters, digits and -", name)
	}
	switch o.ingress {
	case "public", "tunnel", "none":
	default:
		return fmt.Errorf("--ingress: public, tunnel or none, not %q", o.ingress)
	}
	file, all, err := servers()
	if err != nil {
		return err
	}
	known, again := all[name]
	tunnelUser := "hakobu-tunnel@" + sshHost(o.ssh)
	if again && known.SSH != "" && known.SSH != tunnelUser {
		return fmt.Errorf("this laptop knows %s as %s already: pick another name", name, strings.TrimPrefix(known.SSH, "hakobu-tunnel@"))
	}

	env := map[string]string{"HAKOBU_NAME": name, "HAKOBU_INGRESS": o.ingress, "HAKOBU_RESULT": "json"}
	if env["HAKOBU_OWNER_KEY"], err = ownerKey(o.ownerKey); err != nil {
		return err
	}
	if o.email != "" {
		env["HAKOBU_ACME_EMAIL"] = o.email
	}
	if o.ingress == "tunnel" {
		if env["CLOUDFLARE_TUNNEL_TOKEN"], err = secretFromEnv("CLOUDFLARE_TUNNEL_TOKEN", "Cloudflare tunnel token: "); err != nil {
			return err
		}
	}
	var keyFile string
	if o.tailscale {
		env["HAKOBU_API"] = "tailnet"
		if env["TS_AUTHKEY"], err = secretFromEnv("TS_AUTHKEY", "Tailscale auth key: "); err != nil {
			return err
		}
		if o.tags != "" {
			env["TS_TAGS"] = o.tags
		}
	} else {
		env["HAKOBU_API"] = "loopback"
		var pub string
		if keyFile, pub, err = tunnelKey(name); err != nil {
			return err
		}
		env["HAKOBU_TUNNEL_KEY"] = pub
	}
	switch {
	case o.binary != "":
	case o.version != "":
		env["HAKOBU_VERSION"] = update.Tag(o.version)
	case update.Newer(update.Tag(version), "v0.0.0"):
		env["HAKOBU_VERSION"] = update.Tag(version) // the server as this CLI
	}

	bundle, err := installBundle(env, o.binary)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Installing hakobu on %s over SSH (a few minutes on a new server)…\n", o.ssh)
	res, err := runInstall(ctx, o.ssh, bundle, out)
	if err != nil {
		return err
	}
	if res.Name != name {
		return fmt.Errorf("that server is %s already, not %s: nothing remembered", res.Name, name)
	}

	sv := client.Server{URL: "http://" + res.API, Token: res.OperatorToken}
	if res.Mode == "loopback" {
		sv.SSH, sv.SSHKey = tunnelUser, keyFile
	}
	if sv.Token == "" {
		if !again {
			return fmt.Errorf("hakobu was on %s already, and it showed its operator token when it was installed: hakobu server add %s %s", name, name, sv.URL)
		}
		sv.Token = known.Token
	}
	all[name] = sv
	cl, err := all.Client(name)
	if err != nil {
		return err
	}
	if err := waitAPI(ctx, cl, name); err != nil {
		return err
	}
	if err := all.Save(file); err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s runs hakobu %s; this laptop reaches its API", name, res.Version)
	if sv.SSH != "" {
		fmt.Fprintf(out, " through SSH, with %s.\n", keyFile)
	} else {
		fmt.Fprintf(out, " at %s, on the tailnet.\n", sv.URL)
	}
	if res.AdminToken != "" {
		fmt.Fprintf(out, "\nThe admin token, shown only now: put it in your password manager, never on a disk an agent reads.\n  %s\n", res.AdminToken)
	}
	fmt.Fprintln(out, "\nNext: hakobu status, then hakobu init --server", name, "in a project.")
	return nil
}

// sshHost is the host of user@host.
func sshHost(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ownerKey is the person's age public key: the flag's, the one kept, or
// one asked for at the terminal, which is kept then.
func ownerKey(flag string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, "hakobu", "owner.pub")
	key := flag
	if key == "" {
		if b, err := os.ReadFile(file); err == nil {
			key = strings.TrimSpace(string(b))
		}
	}
	asked := false
	if key == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", fmt.Errorf("no age public key: give --owner-key age1…, or put it in %s (age-keygen makes a pair; its secret half goes in your password manager)", file)
		}
		fmt.Fprintln(os.Stderr, "Secrets and backups are sealed to your age key too, so you can open them without the server.")
		fmt.Fprintln(os.Stderr, "age-keygen makes a pair: keep its secret half in your password manager.")
		fmt.Fprint(os.Stderr, "Your age public key (age1…): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		key, asked = strings.TrimSpace(line), true
	}
	if _, err := age.ParseX25519Recipient(key); err != nil {
		return "", fmt.Errorf("%q isn't an age public key (age1…)", key)
	}
	if _, err := os.Stat(file); asked || (flag != "" && errors.Is(err, os.ErrNotExist)) {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(file, []byte(key+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	return key, nil
}

// secretFromEnv is a secret from the environment, or typed, hidden: never
// an argument.
func secretFromEnv(name, prompt string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("set %s, or run this at a terminal to type it", name)
	}
	return readSecret(prompt)
}

// tunnelKey is the server's SSH key on this laptop, made the first time:
// the private half's file, and the public half for the server.
func tunnelKey(name string) (string, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	file := filepath.Join(dir, "hakobu", "keys", name)
	if b, err := os.ReadFile(file); err == nil {
		k, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", file, err)
		}
		return file, authorizedKey(k.PublicKey(), name), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hakobu "+name)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return file, authorizedKey(sp, name), nil
}

func authorizedKey(k ssh.PublicKey, name string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " hakobu@" + name
}

// installBundle is what goes in on ssh's stdin: a tar of install.sh, its
// settings first, and a local build if there's one.
func installBundle(env map[string]string, binary string) ([]byte, error) {
	var script strings.Builder
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&script, "export %s=%s\n", k, shellQuote(env[k]))
	}
	script.WriteString(InstallScript)

	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add := func(name string, mode int64, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("install.sh", 0o600, []byte(script.String())); err != nil {
		return nil, err
	}
	if binary != "" {
		data, err := os.ReadFile(binary)
		if err != nil {
			return nil, err
		}
		if err := add("hakobu", 0o700, data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// remoteInstall unpacks the bundle in a directory of root's own and runs
// install.sh from there, as root or through sudo.
const remoteInstall = `set -e; d="$(mktemp -d)"; trap 'rm -rf "$d"' EXIT; tar -x -o -C "$d"; if [ -f "$d/hakobu" ]; then export HAKOBU_BINARY="$d/hakobu"; fi; bash "$d/install.sh" </dev/null`

func runInstall(ctx context.Context, target string, bundle []byte, out io.Writer) (installResult, error) {
	var res installResult
	q := shellQuote(remoteInstall)
	remote := `if [ "$(id -u)" -eq 0 ]; then sh -c ` + q + `; else sudo -n sh -c ` + q + `; fi`
	// A new server's host key is taken as it comes, as ssh's own prompt
	// would; after that it must stay the same.
	cmd := exec.CommandContext(ctx, client.SSHCommand, "-o", "StrictHostKeyChecking=accept-new", target, remote)
	cmd.Stdin = bytes.NewReader(bundle)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, err
	}
	found := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if j, ok := strings.CutPrefix(line, "HAKOBU_RESULT "); ok {
			if err := json.Unmarshal([]byte(j), &res); err != nil {
				return res, fmt.Errorf("the installer's result: %w", err)
			}
			found = true
			continue
		}
		fmt.Fprintln(out, "  "+line)
	}
	if err := cmd.Wait(); err != nil {
		return res, fmt.Errorf("installing over ssh %s: %w (above, what it said)", target, err)
	}
	if !found {
		return res, errors.New("the installer finished without its result: is that server's hakobu older than this CLI's?")
	}
	return res, nil
}

// waitAPI waits for the server's API to answer, through the way the
// laptop will reach it, and checks it's the server meant.
func waitAPI(ctx context.Context, cl *client.Client, name string) error {
	deadline := time.Now().Add(apiWait)
	for {
		var key map[string]string
		err := cl.Get(ctx, "/v1/key", &key)
		if err == nil {
			if key["server"] != name {
				return fmt.Errorf("the API answers as %s, not %s", key["server"], name)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hakobu is installed, but its API doesn't answer: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
