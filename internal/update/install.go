package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// hakobu can't update itself: it runs as the unprivileged user hakobu,
// which can't write its binary, so a break-in into it can't plant a binary
// of its own. It asks by creating RequestFile in its data directory;
// systemd (hakobu-update.path, written by install.sh) then runs
// `hakobu server update --requested` as root, which installs only a newer
// signed release. The worst a forged request does is that.

// RequestFile, in the data directory, asks for an update: empty for the
// latest release, or a tag.
const RequestFile = "update-request"

// Files of an install, in its directory; all root's.
const (
	binaryFile   = "hakobu"
	prevFile     = "hakobu.prev"        // the binary before the last update
	downloadFile = "hakobu.download"    // a release being checked and installed
	scriptFile   = "install.sh"         // its installer
	StatusFile   = "update-status.json" // the last update, for the daemon to show
	lockFile     = "update.lock"
)

// Install is hakobu as install.sh set it up on this server.
type Install struct {
	Dir     string    // /opt/hakobu
	Data    string    // /var/lib/hakobu
	Version string    // of the binary installed now: the one running this
	Listen  string    // the API's address, which a version that's up answers
	Out     io.Writer // progress, and the installer's

	// For tests: run a command, and tell whether hakobu is up.
	run     func(stream bool, name string, args ...string) (string, error)
	healthy func(version string) bool
	wait    time.Duration // how long a new version has to come up
}

// Status is the last update, for the daemon to show.
type Status struct {
	At      time.Time `json:"at"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	State   string    `json:"state"` // "running", "updated", "up to date", "failed", "rolled back"
	Message string    `json:"message,omitempty"`
}

// ReadStatus reads the last update's status; ok is false if there was none.
func ReadStatus(dir string) (st Status, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

// ReadRequest takes the update request out of the data directory: the tag
// it names, or "" for the latest. The directory is hakobu's, so the
// request is read as what it could be, anything: a symlink to a file only
// root can read is refused, not followed, and what's in it is never shown.
func ReadRequest(data string) (string, error) {
	p := filepath.Join(data, RequestFile)
	defer os.Remove(p)
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("the update request isn't a file")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("the update request isn't a file")
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64))
	tag := strings.TrimSpace(string(b))
	if tag == "" || tag == "latest" {
		return "", nil
	}
	if _, ok := parseTag(tag); !ok {
		return "", errors.New("the update request names no release (like v1.2.3)")
	}
	return Tag(tag), nil
}

func (in *Install) path(name string) string { return filepath.Join(in.Dir, name) }

func (in *Install) logf(format string, args ...any) {
	fmt.Fprintf(in.Out, format+"\n", args...)
}

// lock keeps two updates (a requested one and one by hand) from running at
// once.
func (in *Install) lock() (unlock func(), err error) {
	f, err := os.OpenFile(in.path(lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another update is running")
	}
	return func() { f.Close() }, nil
}

// Update installs release tag, or the latest if tag is "", if it's newer
// than the installed one: its signed binary, through its own installer, so
// an update does what running install.sh by hand does. If the new version
// doesn't come up, the previous binary is put back.
func (in *Install) Update(tag string) error {
	unlock, err := in.lock()
	if err != nil {
		return err
	}
	defer unlock()
	from := Tag(in.Version)
	if tag == "" {
		latest, err := Latest()
		if err != nil {
			in.setStatus(from, "failed", "", err.Error())
			return err
		}
		if !Newer(latest, in.Version) {
			in.logf("hakobu %s is the latest release", from)
			in.setStatus(from, "up to date", latest, "")
			return nil
		}
		tag = latest
	}
	if !Newer(tag, in.Version) {
		err := fmt.Errorf("hakobu %s runs here: an update only goes to a newer release than that, not %s", from, tag)
		in.setStatus(from, "failed", tag, err.Error())
		return err
	}
	fail := func(err error) error {
		os.Remove(in.path(downloadFile))
		in.setStatus(from, "failed", tag, err.Error())
		return err
	}
	in.setStatus(from, "running", tag, "downloading and checking "+tag)
	in.logf("downloading hakobu %s and checking its signature", tag)
	if err := Download(tag, in.path(downloadFile)); err != nil {
		return fail(err)
	}
	script, err := in.command(false, in.path(downloadFile), "server", "installer")
	if err != nil || !strings.HasPrefix(script, "#!") {
		return fail(fmt.Errorf("hakobu %s carries no installer: update it with install.sh", tag))
	}
	// A file, not stdin: what the installer runs (apt, Tailscale's
	// installer) could read the rest of it from there.
	if err := os.WriteFile(in.path(scriptFile), []byte(script), 0o700); err != nil {
		return fail(err)
	}
	defer os.Remove(in.path(scriptFile))
	if err := copyFile(in.path(binaryFile), in.path(prevFile)); err != nil {
		return fail(err)
	}

	in.setStatus(from, "running", tag, "installing "+tag)
	in.logf("installing hakobu %s with its installer", tag)
	_, err = in.command(true, "env", "HAKOBU_BINARY="+in.path(downloadFile), "bash", in.path(scriptFile))
	if err == nil && in.up(tag) {
		os.Remove(in.path(downloadFile))
		in.logf("hakobu %s is up", tag)
		in.setStatus(from, "updated", tag, "")
		return nil
	}
	why := "didn't come up (journalctl -u hakobu)"
	if err != nil {
		why = "its installer failed: " + err.Error()
	}
	in.logf("hakobu %s %s; going back to %s", tag, why, from)
	if err := in.putBack(from); err != nil {
		err = fmt.Errorf("hakobu %s %s, and going back to %s failed: %w", tag, why, from, err)
		in.setStatus(from, "failed", tag, err.Error())
		return err
	}
	os.Remove(in.path(downloadFile))
	err = fmt.Errorf("hakobu %s %s, so %s was put back", tag, why, from)
	in.setStatus(from, "rolled back", tag, err.Error())
	return err
}

// putBack installs the binary from before the update again and restarts
// hakobu with it. What the new installer changed besides (its units, Docker's
// settings) stays: the next release's installer writes them again.
func (in *Install) putBack(version string) error {
	if err := copyFile(in.path(prevFile), in.path(binaryFile)+".new"); err != nil {
		return err
	}
	if err := os.Rename(in.path(binaryFile)+".new", in.path(binaryFile)); err != nil {
		return err
	}
	if err := in.restartDialer(); err != nil {
		in.logf("restarting the dialer: %v", err)
	}
	if _, err := in.command(false, "systemctl", "restart", "hakobu"); err != nil {
		return err
	}
	if !in.up(version) {
		return fmt.Errorf("hakobu %s didn't come up either: see journalctl -u hakobu", version)
	}
	in.logf("hakobu %s is back", version)
	return nil
}

// up waits for version to be the binary installed, its service to be
// running and its API to answer, and to still be so a little later: one
// that crashes on its first deploy cycle restarts in a loop.
func (in *Install) up(version string) bool {
	if in.healthy != nil {
		return in.healthy(version)
	}
	wait := in.wait
	if wait == 0 {
		wait = 3 * time.Minute
	}
	check := func() bool {
		v, err := in.command(false, in.path(binaryFile), "--version")
		if err != nil || !strings.HasSuffix(strings.TrimSpace(v), " "+strings.TrimPrefix(version, "v")) {
			return false
		}
		if s, _ := in.command(false, "systemctl", "is-active", "hakobu"); strings.TrimSpace(s) != "active" {
			return false
		}
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + in.Listen + "/v1/key")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if check() {
			time.Sleep(20 * time.Second)
			return check()
		}
	}
	return false
}

func (in *Install) setStatus(from, state, to, msg string) {
	b, _ := json.Marshal(Status{At: time.Now().UTC(), From: from, To: to, State: state, Message: msg})
	tmp := in.path(StatusFile) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		err = os.Rename(tmp, in.path(StatusFile))
		if err == nil {
			return
		}
	}
	in.logf("couldn't save the update's status")
}

// command runs name and returns its output; streamed, it goes to Out as
// it comes too.
func (in *Install) command(stream bool, name string, args ...string) (string, error) {
	if in.run != nil {
		return in.run(stream, name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = "/"
	var out strings.Builder
	if stream {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(in.Out, &out), io.MultiWriter(in.Out, &out)
	} else {
		cmd.Stdout, cmd.Stderr = &out, &out
	}
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(out.String())
		if i := strings.LastIndex(tail, "\n"); i >= 0 {
			tail = tail[i+1:]
		}
		return out.String(), fmt.Errorf("%s: %w: %s", name, err, tail)
	}
	return out.String(), nil
}

// restartDialer restarts the Docker user's dialer, which runs the hakobu
// binary too; systemd before 248 can't reach another user's services with
// -M, so it's done as that user then.
func (in *Install) restartDialer() error {
	if _, err := in.command(false, "systemctl", "--user", "-M", "hakobu-docker@", "restart", "hakobu-dialer"); err == nil {
		return nil
	}
	u, err := user.Lookup("hakobu-docker")
	if err != nil {
		return err
	}
	run := "/run/user/" + u.Uid
	_, err = in.command(false, "runuser", "-u", "hakobu-docker", "--", "env", "XDG_RUNTIME_DIR="+run, "DBUS_SESSION_BUS_ADDRESS=unix:path="+run+"/bus",
		"systemctl", "--user", "restart", "hakobu-dialer")
	return err
}

// copyFile copies src to dst (mode 0755) through a file beside it.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
