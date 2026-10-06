package ops

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Servers: nodes on other servers join the panel with a one-time token
// and then connect over the link (internal/link) with their own key. The
// panel calls each one as a node.Remote while it's connected.

// joinValid is how long a join token works.
const joinValid = time.Hour

// linkKey is the panel's key on the link, made the first time.
func linkKey(s *store.Store) (ed25519.PrivateKey, error) {
	stored, err := s.GetLinkKey(ctx())
	if errors.Is(err, sql.ErrNoRows) {
		key, err := link.NewKey()
		if err != nil {
			return nil, err
		}
		if err := s.SaveLinkKey(ctx(), secret.String(base64.StdEncoding.EncodeToString(key))); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(string(stored))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("the panel's link key is corrupt")
	}
	return ed25519.PrivateKey(raw), nil
}

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// AddServer makes a server of user's the panel will take as name and
// returns the token it joins with; for a server that hasn't joined yet, a
// new token.
func AddServer(s *store.Store, userID int64, name string) (string, error) {
	if err := checkName("server", name); err != nil {
		return "", err
	}
	key, err := linkKey(s)
	if err != nil {
		return "", err
	}
	tok, err := link.NewToken(key)
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(joinValid).UTC().Format(time.RFC3339)
	n, err := s.GetNodeByName(ctx(), store.GetNodeByNameParams{UserID: userID, Name: name})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = s.CreateNode(ctx(), store.CreateNodeParams{Name: name, JoinSecretHash: secretHash(tok.Secret), JoinExpires: expires, UserID: userID})
	case err != nil:
	case n.PublicKey != "":
		return "", fmt.Errorf("server %s has joined already", name)
	default:
		err = s.SetNodeJoin(ctx(), store.SetNodeJoinParams{JoinSecretHash: secretHash(tok.Secret), JoinExpires: expires, ID: n.ID})
	}
	return tok.String(), err
}

// UserServer is the user's server by its name.
func UserServer(s *store.Store, userID int64, name string) (store.Node, error) {
	n, err := s.GetNodeByName(ctx(), store.GetNodeByNameParams{UserID: userID, Name: name})
	if err != nil {
		return n, fmt.Errorf("server %q not found", name)
	}
	return n, nil
}

// RemoveServer forgets a server that runs no project, deleting its
// tunnels, and drops its link; its key no longer admits it.
func RemoveServer(s *store.Store, id int64) error {
	n, err := s.GetNode(ctx(), id)
	if err != nil {
		return fmt.Errorf("server not found")
	}
	if projects, _ := s.ProjectsOnNode(ctx(), sql.NullInt64{Int64: n.ID, Valid: true}); len(projects) > 0 {
		return fmt.Errorf("server %s still runs %s: delete them first", n.Name, strings.Join(projects, ", "))
	}
	for _, t := range tunnelsOn(s, server{ID: n.ID, Name: n.Name}) {
		dropServerTunnel(s, t)
	}
	if err := s.DeleteNode(ctx(), n.ID); err != nil {
		return err
	}
	links.drop(n.ID)
	return nil
}

// dropServerTunnel deletes a tunnel on a server that joined: its
// cloudflared if the server is connected, the tunnel in Cloudflare, and
// the record. What can't be deleted is left behind, said so.
func dropServerTunnel(s *store.Store, t tunnel) {
	if err := t.server.node().RemoveTunnel(ctx(), t.acct.Name); err != nil {
		panellog.Warnf("%s: its cloudflared is left on the server: %v", t.label(), err)
	}
	if err := t.acct.Client.DeleteTunnel(t.acct.AccountID, t.ID); err != nil {
		panellog.Warnf("%s: delete it in Cloudflare: %v", t.label(), err)
	}
	if err := s.DeleteServerTunnel(ctx(), t.row); err != nil {
		panellog.Errorf("%s: %v", t.label(), err)
	}
}

// admit takes a node by its key, or the first time by its token's secret.
func admit(s *store.Store, key ed25519.PublicKey, joinSecret, version string) (string, error) {
	n, err := s.GetNodeByKey(ctx(), hex.EncodeToString(key))
	if err == nil {
		return linkName(n.ID), s.SeeNode(ctx(), store.SeeNodeParams{Version: version, LastSeen: now(), ID: n.ID})
	}
	if joinSecret == "" {
		return "", errors.New("this server hasn't joined the panel")
	}
	n, err = s.GetNodeByJoinSecret(ctx(), secretHash(joinSecret))
	if err != nil {
		return "", errors.New("the join token isn't valid: make a new one in the panel")
	}
	if expires, err := time.Parse(time.RFC3339, n.JoinExpires); err != nil || time.Now().After(expires) {
		return "", errors.New("the join token has expired: make a new one in the panel")
	}
	if err := s.JoinNode(ctx(), store.JoinNodeParams{PublicKey: hex.EncodeToString(key), ID: n.ID}); err != nil {
		return "", err
	}
	return linkName(n.ID), s.SeeNode(ctx(), store.SeeNodeParams{Version: version, LastSeen: now(), ID: n.ID})
}

// linkName is what the link calls a server: its ID, as names are only
// unique among a user's servers.
func linkName(id int64) string { return strconv.FormatInt(id, 10) }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// links are the nodes connected now, by ID.
var links = &linkSet{m: map[int64]*linked{}}

type linkSet struct {
	mu sync.Mutex
	m  map[int64]*linked
}

type linked struct {
	remote  *node.Remote
	session *yamux.Session
	version string
	since   time.Time
}

func (l *linkSet) get(id int64) (*linked, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.m[id]
	return c, ok
}

func (l *linkSet) drop(id int64) {
	l.mu.Lock()
	c := l.m[id]
	delete(l.m, id)
	l.mu.Unlock()
	if c != nil && c.session != nil {
		c.session.Close()
	}
}

// linkWork is what the panel does for connected servers, for tests to wait
// on once their servers are gone.
var linkWork sync.WaitGroup

// LinkServer takes nodes' connections at link.Path. ingest takes the
// errors, logs and traces a server's apps send (POST
// /api/{app_id}/envelope/), passed on by the server over its link.
func LinkServer(s *store.Store, version string) (http.Handler, error) {
	key, err := linkKey(s)
	if err != nil {
		return nil, err
	}
	watchPanelURL.Do(func() { go tellPanelMoves(s, 10*time.Second) })
	return &link.Server{
		Key: key, Version: version, URL: panelAddress,
		Admit: func(k ed25519.PublicKey, joinSecret, v string) (string, error) { return admit(s, k, joinSecret, v) },
		Serve: func(ln, v string, sess *yamux.Session) {
			id, _ := strconv.ParseInt(ln, 10, 64)
			n, err := s.GetNode(ctx(), id)
			if err != nil {
				sess.Close()
				return
			}
			linkWork.Add(1)
			defer linkWork.Done()
			sv := server{ID: n.ID, Name: n.Name}
			r := node.NewRemote(sess.Open)
			c := &linked{remote: r, session: sess, version: v, since: time.Now()}
			links.mu.Lock()
			old := links.m[id]
			links.m[id] = c
			links.mu.Unlock()
			if old != nil {
				old.session.Close() // the node came back on a new connection
			}
			panellog.Info(sv.label(), "connected, hakobu", v)
			go func() { _ = http.Serve(sess, fromServer(s, id, r)) }()
			watching, stopWatching := context.WithCancel(context.Background())
			linkWork.Add(1)
			go func() {
				defer linkWork.Done()
				serverConnected(s, sv, r, watching)
			}()
			<-sess.CloseChan()
			stopWatching()
			links.mu.Lock()
			if links.m[id] == c {
				delete(links.m, id)
			}
			links.mu.Unlock()
			_ = s.SeeNode(ctx(), store.SeeNodeParams{Version: v, LastSeen: now(), ID: id})
			panellog.Info(sv.label(), "disconnected")
		},
	}, nil
}

// panelAddress is the panel's address for servers, "" before setup.
func panelAddress() string {
	if host := config.PublicHost(); host != "" {
		return "https://" + host
	}
	return ""
}

var watchPanelURL sync.Once

// tellPanelMoves follows `hakobu setup --domain` changing the panel's
// address: it tells the connected servers, while their links still stand
// (the old address may already be gone; servers away learn it when they
// connect, if they still can), then restarts the apps, whose
// SENTRY_PUBLIC_DSN has the panel's address in it.
func tellPanelMoves(s *store.Store, every time.Duration) {
	last := panelAddress()
	for range time.Tick(every) {
		url := panelAddress()
		if url == last || url == "" {
			continue
		}
		last = url
		tellServersPanelURL(url)
		restartForPanel(s)
	}
}

func restartForPanel(s *store.Store) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		panellog.Warn("apps not restarted for the panel's new address:", err)
		return
	}
	for _, app := range apps {
		if err := restartApp(s, app.ID); err != nil {
			panellog.Warnf("%s not restarted for the panel's new address (deploy it again): %v", app.Name, err)
			continue
		}
		panellog.Info("restarted", app.Name, "for the panel's new address")
	}
}

func tellServersPanelURL(url string) {
	links.mu.Lock()
	connected := maps.Clone(links.m)
	links.mu.Unlock()
	for id, c := range connected {
		if err := c.remote.TellPanelURL(ctx(), url); err != nil {
			panellog.Warn("server", id, "wasn't told the panel's new address:", err)
			continue
		}
		panellog.Info("told server", id, "the panel is at", url)
	}
}

// Server is a server as the panel shows it.
type Server struct {
	ID        int64
	UserID    int64 // who added it
	Name      string
	Joined    bool
	Connected bool
	Version   string
	LastSeen  string // when it last connected or went, "" if never
	// Its keys, once it has told them: Key.Exported says the owner has a
	// copy of the current one (KeyExportCommand).
	Key      node.KeyState
	KeyKnown bool
}

func Servers(s *store.Store) ([]Server, error) {
	rows, err := s.ListNodes(ctx())
	if err != nil {
		return nil, err
	}
	out := make([]Server, len(rows))
	for i, n := range rows {
		out[i] = Server{ID: n.ID, UserID: n.UserID, Name: n.Name, Joined: n.PublicKey != "", Version: n.Version, LastSeen: n.LastSeen}
		if c, ok := links.get(n.ID); ok {
			out[i].Connected, out[i].Version = true, c.version
		}
		out[i].Key, out[i].KeyKnown = knownServerKeys(n.ID)
	}
	return out, nil
}

// serverNode is the connected server's node, ErrUnreachable while it
// isn't connected.
func serverNode(sv server) (node.Node, error) {
	if c, ok := links.get(sv.ID); ok {
		return c.remote, nil
	}
	return nil, fmt.Errorf("%w: %s isn't connected", node.ErrUnreachable, sv.label())
}

// serverConnected brings a server that just connected up to date (it may
// have restarted, or missed changes while away) and watches it while it
// stays: its tunnels and networks, slots and proxies left by unfinished
// jobs, the routes to its apps, and its dying containers.
func serverConnected(s *store.Store, sv server, n node.Node, watching context.Context) {
	noteServerKeys(sv.ID, n)
	for _, t := range tunnelsOn(s, sv) {
		if err := startTunnel(s, t); err != nil {
			panellog.Errorf("%s: %v", t.label(), err)
		}
	}
	// The relay its apps send errors and traces to; it joins the projects'
	// networks next.
	if err := n.StartIngestRelay(ctx(), IngestSocket()); err != nil {
		panellog.Warn("ingest relay of", sv.label()+":", err)
	}
	if err := ensureServerNetworks(s, sv); err != nil {
		panellog.Warn("networks of", sv.label()+":", err)
	}
	var specs []node.AppSpec
	apps, _ := s.ListApps(ctx())
	places, _ := projectPlaces(s)
	for _, app := range apps {
		if places[app.ProjectID].server == sv.ID && !IsDeploying(app.ID) {
			specs = append(specs, liveSpec(app))
		}
	}
	n.Reconcile(ctx(), specs)
	finishPromotions(s, sv, n)
	for _, spec := range specs {
		n.EnsureProxy(ctx(), spec)
	}
	if err := SyncTunnel(s); err != nil {
		panellog.Warn("tunnel routes not updated (retrying):", err)
	}
	for watching.Err() == nil {
		err := n.WatchDeaths(watching, func(container, app string, d node.Death) { recordDeath(s, sv, container, app, d) })
		if watching.Err() == nil {
			panellog.Warn("docker events of", sv.label()+":", err)
			time.Sleep(5 * time.Second)
		}
	}
}

// IngestApp is what a server learns of an app of its own to take its
// envelopes: its name and the key of its DSN.
type IngestApp struct {
	ID  int64
	Key string
}

// fromServer answers what a server asks of the panel over its link: the
// URLs of its backups' parts, and the DSN keys of its apps, told only for
// apps that run on it: a server can't take another's apps' envelopes.
func fromServer(s *store.Store, nodeID int64, r *node.Remote) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /callback/{id}", r.Handler())
	mux.HandleFunc("GET /ingest/apps/{app_id}", func(w http.ResponseWriter, req *http.Request) {
		id, _ := strconv.ParseInt(req.PathValue("app_id"), 10, 64)
		app, err := s.GetAppByID(ctx(), id)
		if err != nil || !appOnServer(s, app, nodeID) {
			http.Error(w, "not an app of this server", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(IngestApp{app.ID, string(app.SentryKey)})
	})
	return mux
}

// appOnServer reports whether app runs on the server (not the panel's)
// with ID nodeID.
func appOnServer(s *store.Store, app store.App, nodeID int64) bool {
	sv, ok := appServer(s, app)
	return ok && sv.ID == nodeID && nodeID != 0
}

// appServer is the server app runs on.
func appServer(s *store.Store, app store.App) (server, bool) {
	p, err := s.GetProjectByID(ctx(), app.ProjectID)
	if err != nil {
		return server{}, false
	}
	sv, err := projectServer(s, p)
	return sv, err == nil
}

// JoinCommand is what to run, as root, on a new server to install hakobu
// there and join this panel with token.
func JoinCommand(token string) string {
	return "curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_PANEL=https://" + config.PublicHost() + " HAKOBU_JOIN_TOKEN=" + token + " bash"
}
