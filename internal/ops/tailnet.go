package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/tailscale"
)

// Tailnets. A user connects tailnets with an OAuth client each
// (Settings → Tailscale), theirs and a client's say, then puts servers of
// theirs in one, so the apps there reach its devices by their 100.x
// addresses (node/tailnet.go).

// DefaultTailnetTag is the tag a server's key carries unless the user
// names another.
const DefaultTailnetTag = "tag:hakobu"

// Tailnet is a tailnet a user connected, under their name for it.
type Tailnet struct {
	ID     int64
	Name   string
	Client tailscale.Client
}

func tailnetOf(row store.TailscaleTailnet) Tailnet {
	return Tailnet{ID: row.ID, Name: row.Name, Client: tailscale.Client{ID: row.ClientID, Secret: string(row.ClientSecret), Tag: row.Tag}}
}

// Tailnets are the user's tailnets, by name.
func Tailnets(s *store.Store, userID int64) []Tailnet {
	rows, _ := s.ListTailnets(ctx(), userID)
	out := make([]Tailnet, 0, len(rows))
	for _, r := range rows {
		out = append(out, tailnetOf(r))
	}
	return out
}

func userTailnet(s *store.Store, userID int64, name string) (Tailnet, error) {
	row, err := s.GetTailnetByName(ctx(), store.GetTailnetByNameParams{UserID: userID, Name: name})
	if err != nil {
		return Tailnet{}, fmt.Errorf("tailnet %q not found", name)
	}
	return tailnetOf(row), nil
}

// AddTailnet checks and keeps an OAuth client of a tailnet of the user's,
// under name.
func AddTailnet(s *store.Store, userID int64, name, id, sec, tag string) error {
	name = strings.TrimSpace(name)
	if err := checkName("tailnet", name); err != nil {
		return err
	}
	c := tailscale.Client{ID: strings.TrimSpace(id), Secret: strings.TrimSpace(sec), Tag: strings.TrimSpace(tag)}
	if c.Tag == "" {
		c.Tag = DefaultTailnetTag
	}
	if c.ID == "" || c.Secret == "" {
		return errors.New("the client ID and secret are both needed")
	}
	if _, err := userTailnet(s, userID, name); err == nil {
		return fmt.Errorf("a tailnet named %q already exists", name)
	}
	cctx, cancel := context.WithTimeout(ctx(), 30*time.Second)
	defer cancel()
	if err := c.Check(cctx); err != nil {
		return err
	}
	_, err := s.CreateTailnet(ctx(), store.CreateTailnetParams{UserID: userID, Name: name, ClientID: c.ID, ClientSecret: secret.String(c.Secret), Tag: c.Tag})
	return err
}

// RenameTailnet gives a tailnet of the user's another name, a label.
func RenameTailnet(s *store.Store, userID int64, name, to string) error {
	tn, err := userTailnet(s, userID, name)
	if err != nil {
		return err
	}
	to = strings.TrimSpace(to)
	if err := checkName("tailnet", to); err != nil {
		return err
	}
	if err := s.RenameTailnet(ctx(), store.RenameTailnetParams{Name: to, ID: tn.ID}); err != nil {
		return taken(err, "a tailnet named %q", to)
	}
	return nil
}

// ForgetTailnet forgets a tailnet's OAuth client. Servers in it stay
// there; hakobu just no longer sees it.
func ForgetTailnet(s *store.Store, userID int64, name string) error {
	tn, err := userTailnet(s, userID, name)
	if err != nil {
		return err
	}
	forgetDevices(tn.Client)
	return s.DeleteTailnet(ctx(), tn.ID)
}

// ServerTailnet is a server's place in its owner's tailnet, for its row.
type ServerTailnet struct {
	node.TailnetState
	Known    bool   // the server told its state
	Device   bool   // its device was found in the tailnet
	Tailnet  string   // the name of the one it was found in
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

// TailnetOf is where server n stands in its owner's tailnets. A server
// put in one by hand is taken as it is: hakobu finds its device by its
// address, in the tailnet it was last in first.
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
	if out.IP == "" {
		return out
	}
	tailnets := Tailnets(s, n.UserID)
	slices.SortStableFunc(tailnets, func(a, b Tailnet) int { // its own first
		return cmpBool(b.ID == n.TailnetID.Int64, a.ID == n.TailnetID.Int64)
	})
	for _, tn := range tailnets {
		devices, err := devicesOf(tn.Client)
		if err != nil {
			continue
		}
		for _, d := range devices {
			if d.IPv4() != out.IP {
				continue
			}
			out.Device, out.Tailnet, out.Online, out.LastSeen, out.Tags = true, tn.Name, d.Online, d.LastSeen, d.Tags
			if d.NodeID != n.TailnetDevice || out.IP != n.TailnetIP || n.TailnetID.Int64 != tn.ID {
				_ = s.SetNodeTailnet(ctx(), store.SetNodeTailnetParams{TailnetID: sql.NullInt64{Int64: tn.ID, Valid: true}, TailnetDevice: d.NodeID, TailnetIP: out.IP, ID: n.ID})
			}
			return out
		}
	}
	return out
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// ConnectServerTailnet puts a server of the user's in their tailnet named
// tailnet ("" when they have one) with tag ("" for its client's), so the
// tailnet's policy can tell servers apart: a one-off key for it, which
// root on the server joins with.
func ConnectServerTailnet(s *store.Store, userID int64, serverName, tailnet, tag string) error {
	var tn Tailnet
	switch all := Tailnets(s, userID); {
	case len(all) == 0:
		return errors.New("connect a tailnet first (Settings → Tailscale)")
	case tailnet == "" && len(all) == 1:
		tn = all[0]
	case tailnet == "":
		return errors.New("pick one of your tailnets")
	default:
		var err error
		if tn, err = userTailnet(s, userID, tailnet); err != nil {
			return err
		}
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
		tag = tn.Client.Tag
	}
	key, err := tn.Client.AuthKey(cctx, "hakobu server "+n.Name, tag)
	if err != nil {
		return err
	}
	forgetDevices(tn.Client)
	// Its device is found there once it has joined.
	if err := s.SetNodeTailnet(ctx(), store.SetNodeTailnetParams{TailnetID: sql.NullInt64{Int64: tn.ID, Valid: true}, ID: n.ID}); err != nil {
		return err
	}
	return nd.JoinTailnet(cctx, node.TailnetRequest{Key: key, Hostname: "hakobu-" + n.Name, Tag: tag})
}

// DisconnectServerTailnet takes a server of the user's out of its tailnet:
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
	TailnetOf(s, n) // which tailnet, and its device there
	if n, err = UserServer(s, userID, serverName); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx(), 30*time.Second)
	defer cancel()
	if err := nd.LeaveTailnet(cctx); err != nil {
		return err
	}
	if n.TailnetID.Valid && n.TailnetDevice != "" {
		if row, err := s.GetTailnet(ctx(), n.TailnetID.Int64); err == nil {
			tn := tailnetOf(row)
			if err := tn.Client.DeleteDevice(cctx, n.TailnetDevice); err != nil {
				return fmt.Errorf("server %s logs out, but its device stays in tailnet %s: %w", n.Name, tn.Name, err)
			}
			forgetDevices(tn.Client)
		}
	}
	return s.SetNodeTailnet(ctx(), store.SetNodeTailnetParams{ID: n.ID})
}
