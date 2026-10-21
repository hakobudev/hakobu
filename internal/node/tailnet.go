package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"

	"github.com/hakobudev/hakobu/internal/tailscale"
)

// A server in its owner's tailnet: Tailscale runs on the server itself and
// every container reaches the tailnet's 100.x addresses through the host's
// tailscale0, as rootless Docker's outbound connections are the host's.
//
// hakobu can't install or start Tailscale itself (it runs without root),
// so as with updates (internal/update) it asks: it writes
// TailnetRequestFile and systemd runs `hakobu tailscale --requested` as
// root. Unlike an update's, this request is read, so root takes nothing
// from it but a one-off auth key, the server's hostname and its tag, each
// checked, or "down"; what Tailscale may do (routes, DNS, SSH) is fixed in
// the root command. Root leaves the outcome in TailnetStateFile.

const (
	TailnetRequestFile = "data/tailscale-request"
	TailnetStateFile   = "data/tailscale-state.json"
	tailnetInterface   = "tailscale0"
)

// TailnetState is a server's place in a tailnet, as the panel sees it.
type TailnetState struct {
	IP      string // its 100.x address, "" when it isn't in one
	Pending string // "up" or "down" while root hasn't done it yet
	Error   string // why the last request failed
	NoTUN   bool   // no /dev/net/tun (an Incus container without it)
}

// TailnetRequest is what root is asked to do.
type TailnetRequest struct {
	Up       bool
	Key      string
	Hostname string
	Tag      string
}

var (
	validAuthKey  = regexp.MustCompile(`^tskey-auth-[A-Za-z0-9-]{1,200}$`)
	validHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func (r TailnetRequest) String() string {
	if !r.Up {
		return "down\n"
	}
	return fmt.Sprintf("up %s %s %s\n", r.Key, r.Hostname, r.Tag)
}

// ParseTailnetRequest reads a request, refusing anything but a well-formed
// one.
func ParseTailnetRequest(s string) (TailnetRequest, error) {
	f := strings.Fields(s)
	switch {
	case len(f) == 1 && f[0] == "down":
		return TailnetRequest{}, nil
	case len(f) != 4 || f[0] != "up":
		return TailnetRequest{}, errors.New("not a tailnet request")
	case !validAuthKey.MatchString(f[1]):
		return TailnetRequest{}, errors.New("not an auth key")
	case !validHostname.MatchString(f[2]):
		return TailnetRequest{}, errors.New("not a hostname")
	case !tailscale.ValidTag.MatchString(f[3]):
		return TailnetRequest{}, errors.New("not a tag")
	}
	return TailnetRequest{Up: true, Key: f[1], Hostname: f[2], Tag: f[3]}, nil
}

// TailnetOutcome is what root leaves in TailnetStateFile.
type TailnetOutcome struct {
	Error string `json:"error,omitempty"`
	NoTUN bool   `json:"no_tun,omitempty"`
}

func (Local) Tailnet(ctx context.Context) (TailnetState, error) {
	var st TailnetState
	st.IP = tailnetIP()
	if b, err := os.ReadFile(TailnetRequestFile); err == nil {
		if r, err := ParseTailnetRequest(string(b)); err == nil && r.Up {
			st.Pending = "up"
		} else {
			st.Pending = "down"
		}
	}
	if b, err := os.ReadFile(TailnetStateFile); err == nil {
		var o TailnetOutcome
		if json.Unmarshal(b, &o) == nil {
			st.Error, st.NoTUN = o.Error, o.NoTUN
		}
	}
	return st, nil
}

// tailnetIP is tailscale0's IPv4 address.
func tailnetIP() string {
	iface, err := net.InterfaceByName(tailnetInterface)
	if err != nil {
		return ""
	}
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

func (Local) JoinTailnet(ctx context.Context, r TailnetRequest) error {
	r.Up = true
	if _, err := ParseTailnetRequest(r.String()); err != nil {
		return err
	}
	return askRoot(r)
}

func (Local) LeaveTailnet(ctx context.Context) error {
	return askRoot(TailnetRequest{})
}

func askRoot(r TailnetRequest) error {
	_ = os.Remove(TailnetStateFile) // a fresh outcome follows
	if err := os.WriteFile(TailnetRequestFile, []byte(r.String()), 0o600); err != nil {
		return fmt.Errorf("asking for Tailscale: %w", err)
	}
	return nil
}
