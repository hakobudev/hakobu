package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hakobudev/hakobu/internal/node"
)

var tailscaleRequested bool

// tailscaleCmd puts this server in its owner's tailnet, or takes it out,
// as the panel asked (node/tailnet.go). systemd runs it as root when the
// request appears. Of the request it takes the key, hostname and tag
// alone; Tailscale's other settings are fixed here, so a request can't
// route the server's traffic elsewhere, change its DNS or open SSH.
var tailscaleCmd = &cobra.Command{
	Use:    "tailscale",
	Short:  "Join or leave the tailnet as the panel asked (as root)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !tailscaleRequested {
			return errors.New("only --requested: use Connect to tailnet in the panel")
		}
		dir, err := installDir()
		if err != nil {
			return err
		}
		reqFile := filepath.Join(dir, node.TailnetRequestFile)
		// data/ is hakobu's: neither file is followed if it's a link.
		f, err := os.OpenFile(reqFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 1024))
		f.Close()
		// Taken before acting, so one that fails isn't retried in a loop.
		if err := takeRequest(dir, node.TailnetRequestFile); err != nil {
			return err
		}
		if readErr != nil {
			return readErr
		}
		var out node.TailnetOutcome
		if r, err := node.ParseTailnetRequest(string(b)); err != nil {
			out.Error = "the request was refused: " + err.Error()
		} else if r.Up {
			out = tailnetUp(cmd.Context(), r)
		} else if err := tailnetDown(cmd.Context()); err != nil {
			out.Error = err.Error()
		}
		data, _ := json.Marshal(out)
		if err := writeNew(filepath.Join(dir, node.TailnetStateFile), data); err != nil {
			return err
		}
		if out.Error != "" {
			return errors.New(out.Error)
		}
		return nil
	},
}

func tailnetUp(ctx context.Context, r node.TailnetRequest) node.TailnetOutcome {
	if fi, err := os.Stat("/dev/net/tun"); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		// Tailscale would run in userspace mode, with no tailscale0 for
		// the containers to route through.
		return node.TailnetOutcome{NoTUN: true, Error: "this server has no /dev/net/tun"}
	}
	if _, err := exec.LookPath("tailscale"); err != nil {
		if err := run(ctx, 10*time.Minute, "sh", "-c", "curl -fsSL https://tailscale.com/install.sh | sh"); err != nil {
			return node.TailnetOutcome{Error: "installing Tailscale: " + err.Error()}
		}
	}
	if err := run(ctx, time.Minute, "systemctl", "enable", "--now", "tailscaled"); err != nil {
		return node.TailnetOutcome{Error: "starting tailscaled: " + err.Error()}
	}
	// --force-reauth: a server still logged in to a tailnet (its logout
	// refused) takes the new key rather than going back there.
	err := run(ctx, 3*time.Minute, "tailscale", "up", "--reset", "--force-reauth",
		"--auth-key="+r.Key, "--hostname="+r.Hostname, "--advertise-tags="+r.Tag,
		"--accept-routes=false", "--accept-dns=false", "--ssh=false", "--timeout=2m")
	if err != nil {
		return node.TailnetOutcome{Error: "joining the tailnet: " + strings.ReplaceAll(err.Error(), r.Key, "tskey-auth-…")}
	}
	return node.TailnetOutcome{}
}

func tailnetDown(ctx context.Context) error {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return nil
	}
	err := run(ctx, time.Minute, "tailscale", "logout")
	if err == nil {
		return nil
	}
	// The tailnet may refuse a logout, one whose key it no longer knows
	// say: the server stops using it all the same, and its device is
	// removed there by the panel or by hand.
	fmt.Fprintln(os.Stderr, "logging out of the tailnet:", err)
	if err := run(ctx, time.Minute, "tailscale", "down"); err != nil {
		return fmt.Errorf("leaving the tailnet: %w", err)
	}
	return nil
}

// writeNew writes a file root creates in a directory of hakobu's: whatever
// is there is removed, and a link put back in the meantime fails the
// write rather than being followed.
func writeNew(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// run runs a command, its output's last lines in the error.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return fmt.Errorf("%w: %s", err, strings.Join(lines, " / "))
}

func init() {
	tailscaleCmd.Flags().BoolVar(&tailscaleRequested, "requested", false, "act on the panel's request")
	rootCmd.AddCommand(tailscaleCmd)
}
