package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/tailscale"
)

// Tailnets. A user connects their tailnet with an OAuth client
// (Settings → Tailscale), then puts servers of theirs in it, so the apps
// there reach its devices by their 100.x addresses (node/tailnet.go).

// DefaultTailnetTag is the tag a server's key carries unless the user
// names another.
const DefaultTailnetTag = "tag:hakobu"

// TailscaleClientOf is the user's OAuth client, if they connected one.
func TailscaleClientOf(s *store.Store, userID int64) (tailscale.Client, bool) {
	row, err := s.GetTailscaleClient(ctx(), userID)
	if err != nil {
		return tailscale.Client{}, false
	}
	return tailscale.Client{ID: row.ClientID, Secret: string(row.ClientSecret), Tag: row.Tag}, true
}

// SetTailscaleClient checks and keeps the user's OAuth client; a secret
// left empty keeps the one saved.
func SetTailscaleClient(s *store.Store, userID int64, id, sec, tag string) error {
	c := tailscale.Client{ID: strings.TrimSpace(id), Secret: strings.TrimSpace(sec), Tag: strings.TrimSpace(tag)}
	if c.Tag == "" {
		c.Tag = DefaultTailnetTag
	}
	if c.Secret == "" {
		old, ok := TailscaleClientOf(s, userID)
		if !ok || old.ID != c.ID {
			return errors.New("the client secret is missing")
		}
		c.Secret = old.Secret
	}
	if c.ID == "" {
		return errors.New("the client ID is missing")
	}
	cctx, cancel := context.WithTimeout(ctx(), 30*time.Second)
	defer cancel()
	if err := c.Check(cctx); err != nil {
		return err
	}
	return s.SaveTailscaleClient(ctx(), store.SaveTailscaleClientParams{UserID: userID, ClientID: c.ID, ClientSecret: secret.String(c.Secret), Tag: c.Tag})
}

// RemoveTailscaleClient forgets the user's OAuth client. Their servers stay
// in the tailnet; hakobu just no longer sees it.
func RemoveTailscaleClient(s *store.Store, userID int64) error {
	return s.DeleteTailscaleClient(ctx(), userID)
}

// ServerTailnet is a server's place in its owner's tailnet, for its row.
type ServerTailnet struct {
	node.TailnetState
	Known    bool   // the server told its state
	Device   bool   // its device was found in the tailnet
	Online   bool     // the device is connected to Tailscale
	LastSeen string   // when it last was, if it isn't
	Tags     []string // the device's tags
}

// tailnetDevices caches each client's device list briefly: a Settings page
// asks for every server.
var tailnetDevices = struct {
	sync.Mutex
	m map[string]cachedDevices
}{m: map[string]cachedDevices{}}

type cachedDevices struct {
	at      time.Time
	devices []tailscale.Device
	err     error
}

func devicesOf(c tailscale.Client) ([]tailscale.Device, error) {
	tailnetDevices.Lock()
	defer tailnetDevices.Unlock()
	if d, ok := tailnetDevices.m[c.ID]; ok && time.Since(d.at) < 30*time.Second {
		return d.devices, d.err
	}
	cctx, cancel := context.WithTimeout(ctx(), 10*time.Second)
	defer cancel()
	devices, err := c.Devices(cctx)
	tailnetDevices.m[c.ID] = cachedDevices{time.Now(), devices, err}
	return devices, err
}

func forgetDevices(c tailscale.Client) {
	tailnetDevices.Lock()
	delete(tailnetDevices.m, c.ID)
	tailnetDevices.Unlock()
}

// TailnetOf is where server n stands in its owner's tailnet. A server
// already in it, put there by hand, is taken as it is: hakobu finds its
// device by its address.
func TailnetOf(s *store.Store, n store.Node) ServerTailnet {
	var out ServerTailnet
	if nd, err := serverNode(server{ID: n.ID, Name: n.Name}); err == nil {
		cctx, cancel := context.WithTimeout(ctx(), 5*time.Second)
		st, err := nd.Tailnet(cctx)
		cancel()
		out.TailnetState, out.Known = st, err == nil
	}
	if !out.Known { // away: where it was last
		out.IP = n.TailnetIP
	}
	ip := out.IP
	c, ok := TailscaleClientOf(s, n.UserID)
	if !ok || ip == "" {
		return out
	}
	devices, err := devicesOf(c)
	if err != nil {
		return out
	}
	for _, d := range devices {
		if d.IPv4() == ip {
			out.Device, out.Online, out.LastSeen, out.Tags = true, d.Online, d.LastSeen, d.Tags
			if d.NodeID != n.TailnetDevice || ip != n.TailnetIP {
				_ = s.SetNodeTailnet(ctx(), store.SetNodeTailnetParams{TailnetDevice: d.NodeID, TailnetIP: ip, ID: n.ID})
			}
			break
		}
	}
	return out
}

// ConnectServerTailnet puts a server of the user's in their tailnet with
// tag ("" for the client's), so the tailnet's policy can tell servers
// apart: a one-off key for it, which root on the server joins with.
func ConnectServerTailnet(s *store.Store, userID int64, serverName, tag string) error {
	c, ok := TailscaleClientOf(s, userID)
	if !ok {
		return errors.New("connect your tailnet first (Settings → Tailscale)")
	}
	n, err := UserServer(s, userID, serverName)
	if err != nil {
		return err
	}
	nd, err := serverNode(server{ID: n.ID, Name: n.Name})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx(), 30*time.Second)
	defer cancel()
	if st, err := nd.Tailnet(cctx); err != nil {
		return err
	} else if st.IP != "" {
		return fmt.Errorf("server %s is in a tailnet already, at %s", n.Name, st.IP)
	}
	if tag = strings.TrimSpace(tag); tag == "" {
		tag = c.Tag
	}
	key, err := c.AuthKey(cctx, "hakobu server "+n.Name, tag)
	if err != nil {
		return err
	}
	forgetDevices(c)
	return nd.JoinTailnet(cctx, node.TailnetRequest{Key: key, Hostname: "hakobu-" + n.Name, Tag: tag})
}

// DisconnectServerTailnet takes a server of the user's out of the tailnet:
// it logs out, and its device is removed.
func DisconnectServerTailnet(s *store.Store, userID int64, serverName string) error {
	n, err := UserServer(s, userID, serverName)
	if err != nil {
		return err
	}
	nd, err := serverNode(server{ID: n.ID, Name: n.Name})
	if err != nil {
		return err
	}
	if n.TailnetDevice == "" { // found by its address
		TailnetOf(s, n)
		if n, err = UserServer(s, userID, serverName); err != nil {
			return err
		}
	}
	cctx, cancel := context.WithTimeout(ctx(), 30*time.Second)
	defer cancel()
	if err := nd.LeaveTailnet(cctx); err != nil {
		return err
	}
	if c, ok := TailscaleClientOf(s, userID); ok && n.TailnetDevice != "" {
		if err := c.DeleteDevice(cctx, n.TailnetDevice); err != nil {
			return fmt.Errorf("server %s logs out, but its device stays in the tailnet: %w", n.Name, err)
		}
		forgetDevices(c)
	}
	return s.SetNodeTailnet(ctx(), store.SetNodeTailnetParams{ID: n.ID})
}
