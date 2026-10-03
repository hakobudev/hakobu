package ops

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/node"
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

// AddServer makes a server the panel will take as name and returns the
// token it joins with; for a server that hasn't joined yet, a new token.
func AddServer(s *store.Store, name string) (string, error) {
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
	n, err := s.GetNodeByName(ctx(), name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = s.CreateNode(ctx(), store.CreateNodeParams{Name: name, JoinSecretHash: secretHash(tok.Secret), JoinExpires: expires})
	case err != nil:
	case n.PublicKey != "":
		return "", fmt.Errorf("server %s has joined already", name)
	default:
		err = s.SetNodeJoin(ctx(), store.SetNodeJoinParams{JoinSecretHash: secretHash(tok.Secret), JoinExpires: expires, Name: name})
	}
	return tok.String(), err
}

// RemoveServer forgets a server and drops its link; its key no longer
// admits it.
func RemoveServer(s *store.Store, name string) error {
	if err := s.DeleteNode(ctx(), name); err != nil {
		return err
	}
	links.drop(name)
	return nil
}

// admit takes a node by its key, or the first time by its token's secret.
func admit(s *store.Store, key ed25519.PublicKey, joinSecret, version string) (string, error) {
	n, err := s.GetNodeByKey(ctx(), hex.EncodeToString(key))
	if err == nil {
		return n.Name, s.SeeNode(ctx(), store.SeeNodeParams{Version: version, LastSeen: now(), ID: n.ID})
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
	return n.Name, s.SeeNode(ctx(), store.SeeNodeParams{Version: version, LastSeen: now(), ID: n.ID})
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// links are the nodes connected now, by name.
var links = &linkSet{m: map[string]*linked{}}

type linkSet struct {
	mu sync.Mutex
	m  map[string]*linked
}

type linked struct {
	remote  *node.Remote
	session *yamux.Session
	version string
	since   time.Time
}

func (l *linkSet) get(name string) (*linked, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.m[name]
	return c, ok
}

func (l *linkSet) drop(name string) {
	l.mu.Lock()
	c := l.m[name]
	delete(l.m, name)
	l.mu.Unlock()
	if c != nil {
		c.session.Close()
	}
}

// LinkServer takes nodes' connections at link.Path.
func LinkServer(s *store.Store, version string) (http.Handler, error) {
	key, err := linkKey(s)
	if err != nil {
		return nil, err
	}
	return &link.Server{
		Key: key, Version: version,
		Admit: func(k ed25519.PublicKey, joinSecret, v string) (string, error) { return admit(s, k, joinSecret, v) },
		Serve: func(name, v string, sess *yamux.Session) {
			r := node.NewRemote(sess.Open)
			c := &linked{remote: r, session: sess, version: v, since: time.Now()}
			links.mu.Lock()
			old := links.m[name]
			links.m[name] = c
			links.mu.Unlock()
			if old != nil {
				old.session.Close() // the node came back on a new connection
			}
			fmt.Println("server", name, "connected, hakobu", v)
			go func() { _ = http.Serve(sess, r.Handler()) }()
			<-sess.CloseChan()
			links.mu.Lock()
			if links.m[name] == c {
				delete(links.m, name)
			}
			links.mu.Unlock()
			if n, err := s.GetNodeByName(ctx(), name); err == nil {
				_ = s.SeeNode(ctx(), store.SeeNodeParams{Version: v, LastSeen: now(), ID: n.ID})
			}
			fmt.Println("server", name, "disconnected")
		},
	}, nil
}

// Server is a server as the panel shows it.
type Server struct {
	Name      string
	Joined    bool
	Connected bool
	Version   string
	LastSeen  string // when it last connected or went, "" if never
}

func Servers(s *store.Store) ([]Server, error) {
	rows, err := s.ListNodes(ctx())
	if err != nil {
		return nil, err
	}
	out := make([]Server, len(rows))
	for i, n := range rows {
		out[i] = Server{Name: n.Name, Joined: n.PublicKey != "", Version: n.Version, LastSeen: n.LastSeen}
		if c, ok := links.get(n.Name); ok {
			out[i].Connected, out[i].Version = true, c.version
		}
	}
	return out, nil
}

// serverNode is the connected server's node, ErrUnreachable while it
// isn't connected.
func serverNode(name string) (node.Node, error) {
	if c, ok := links.get(name); ok {
		return c.remote, nil
	}
	return nil, fmt.Errorf("%w: server %s isn't connected", node.ErrUnreachable, name)
}
