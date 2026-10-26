// Package audit compares the server with what it should be: nothing
// listening where the internet can reach it but what's allowed, no
// containers hakobu doesn't know, security updates installed. It reads the
// machine itself, so it finds what was changed by hand, which a record of
// what hakobu did never would.
package audit

import (
	"fmt"
	"net"
	"os"
	"slices"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// Finding is one thing out of place; Key stays the same while it lasts.
type Finding struct {
	Key     string
	Problem string
}

// The tailnet's addresses: Tailscale's CGNAT range and its IPv6 one.
var tailnet = []*net.IPNet{mustCIDR("100.64.0.0/10"), mustCIDR("fd7a:115c:a1e0::/48")}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// Ports finds sockets listening beyond loopback and the tailnet on ports
// not in allow: a database published by hand, a debug server left on. A
// socket on every address (0.0.0.0, ::) is reachable from the internet
// unless a firewall says otherwise, and this can't see the firewall.
func Ports(tables string, allow []int) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, l := range deploy.Listeners(tables) {
		if l.IP.IsLoopback() || inTailnet(l.IP) || slices.Contains(allow, l.Port) {
			continue
		}
		where := l.IP.String()
		if l.IP.IsUnspecified() {
			where = "every address"
		}
		key := fmt.Sprintf("port:%d", l.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Finding{key, fmt.Sprintf("port %d listens on %s, and it isn't one the server should open", l.Port, where)})
	}
	return out
}

func inTailnet(ip net.IP) bool {
	for _, n := range tailnet {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// HostPorts reads the host's listening sockets.
func HostPorts(allow []int) ([]Finding, error) {
	var tables string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		tables += string(b)
	}
	return Ports(tables, allow), nil
}

// Containers finds running containers hakobu didn't start.
func Containers(running []string, ours func(name string) bool) []Finding {
	var out []Finding
	for _, name := range running {
		if !ours(name) {
			out = append(out, Finding{"container:" + name, fmt.Sprintf("container %s runs, and hakobu didn't start it", name)})
		}
	}
	return out
}

// Upgrades finds the security updates not run for longer than maxAge, by
// the stamp apt's daily job leaves (Debian, Ubuntu); a system without it
// has nothing to say.
func Upgrades(stamp string, now time.Time, maxAge time.Duration) []Finding {
	st, err := os.Stat(stamp)
	if err != nil {
		return nil
	}
	if age := now.Sub(st.ModTime()); age > maxAge {
		return []Finding{{"upgrades", fmt.Sprintf("the system's updates last ran %d days ago: check unattended-upgrades", int(age.Hours()/24))}}
	}
	return nil
}

// AptStamp is where unattended-upgrades marks a run.
const AptStamp = "/var/lib/apt/periodic/upgrade-stamp"

// OwnerKey finds the owner's secret key still on the server, where the
// installer made it: it's there for the person to take, and a server that
// keeps it would hand a thief the backups' second key too.
func OwnerKey(file string) []Finding {
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	return []Finding{{"owner-key", "your owner's key is still on the server: take it into your password manager with ssh -t root@SERVER hakobu server owner-key, in your own terminal"}}
}
