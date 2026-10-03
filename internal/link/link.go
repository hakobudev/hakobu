// Package link is the connection between the panel and a node on another
// server. The node dials the panel (no port is open on the node's
// server): a WebSocket to the panel's address, through the panel's
// Cloudflare tunnel, and inside it TLS 1.3 in which each side shows a
// certificate of its own ed25519 key and checks the other's against the
// one it knows. Cloudflare ends the outer TLS and sees the WebSocket, so
// the inner one is what keeps the variables, passwords and logs that go
// over the link from it. On top, yamux carries many streams both ways,
// and its keepalive keeps the WebSocket from being closed as idle.
//
// A node first joins with a token from the panel: a one-time secret and
// the panel's public key, so the node knows the panel's key before it
// ever connects. The panel then admits the node's key; from then on the
// key alone admits it.
package link

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Path is where the panel takes nodes' connections.
const Path = "/nodes/connect"

// NewKey is a new identity for the panel or a node.
func NewKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// Public is the key's public half.
func Public(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// Token lets a node join: a one-time secret, and the panel's public key so
// the node can check it's talking to the panel from the first connection.
type Token struct {
	Secret   string
	PanelKey ed25519.PublicKey
}

// String is the token as the panel shows it and `hakobu node join` takes it.
func (t Token) String() string {
	return base64.RawURLEncoding.EncodeToString(append([]byte(t.Secret), t.PanelKey...))
}

// ParseToken reads a token from String.
func ParseToken(s string) (Token, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != secretLen+ed25519.PublicKeySize {
		return Token{}, errors.New("not a join token of hakobu's")
	}
	return Token{Secret: string(b[:secretLen]), PanelKey: ed25519.PublicKey(b[secretLen:])}, nil
}

const secretLen = 32

// NewToken is a token with a new secret for the panel's key.
func NewToken(panel ed25519.PrivateKey) (Token, error) {
	b := make([]byte, secretLen/4*3)
	if _, err := rand.Read(b); err != nil {
		return Token{}, err
	}
	return Token{Secret: base64.RawURLEncoding.EncodeToString(b), PanelKey: Public(panel)}, nil
}

// certificate is a self-signed certificate of the key: TLS only carries
// it, each side checks the key in it, not the certificate.
func certificate(k ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, Public(k), k)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, nil
}

// peerKey is the ed25519 key of the certificate the other side showed.
func peerKey(raw [][]byte) (ed25519.PublicKey, error) {
	if len(raw) == 0 {
		return nil, errors.New("no certificate")
	}
	cert, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return nil, err
	}
	key, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an ed25519 key")
	}
	return key, nil
}

// hello is the node's first message inside TLS; welcome the panel's answer.
type hello struct {
	Join    string `json:"join,omitempty"` // the token's secret, the first time
	Version string `json:"version"`
}

type welcome struct {
	Node    string `json:"node,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Server takes nodes' connections on the panel.
type Server struct {
	Key     ed25519.PrivateKey
	Version string // the panel's hakobu version
	// Admit decides on a node by its key and, the first time, its token's
	// secret, and names it.
	Admit func(node ed25519.PublicKey, joinSecret, version string) (name string, err error)
	// Serve runs for as long as the node is connected.
	Serve func(name, version string, s *yamux.Session)
}

func (sv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The panel's server times requests out; a link lasts.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	defer conn.Close()
	name, version, sess, err := sv.accept(conn)
	if err != nil {
		return
	}
	defer sess.Close()
	sv.Serve(name, version, sess)
}

func (sv *Server) accept(conn net.Conn) (name, version string, sess *yamux.Session, err error) {
	cert, err := certificate(sv.Key)
	if err != nil {
		return "", "", nil, err
	}
	tc := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		return "", "", nil, err
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", "", nil, errors.New("the node showed no certificate")
	}
	node, err := peerKey([][]byte{certs[0].Raw})
	if err != nil {
		return "", "", nil, err
	}
	rd := bufio.NewReader(tc)
	var h hello
	if err := readLine(rd, &h); err != nil {
		return "", "", nil, err
	}
	name, admitErr := sv.Admit(node, h.Join, h.Version)
	wel := welcome{Node: name, Version: sv.Version}
	if admitErr != nil {
		wel = welcome{Error: admitErr.Error()}
	}
	if err := json.NewEncoder(tc).Encode(wel); err != nil {
		return "", "", nil, err
	}
	if admitErr != nil {
		return "", "", nil, admitErr
	}
	_ = tc.SetDeadline(time.Time{})
	sess, err = yamux.Server(bufferedConn{tc, rd}, yamuxConfig())
	return name, h.Version, sess, err
}

// Dial connects a node to the panel at panelURL ("https://host"), checking
// it's the panel by its key. joinSecret is the token's, the first time.
// It returns the panel's version.
func Dial(ctx context.Context, panelURL string, key ed25519.PrivateKey, panelKey ed25519.PublicKey, joinSecret, version string) (*yamux.Session, string, error) {
	u := strings.TrimSuffix(panelURL, "/") + Path
	u = "ws" + strings.TrimPrefix(u, "http")
	ws, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return nil, "", err
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	cert, err := certificate(key)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	tc := tls.Client(conn, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		// The panel is known by its key, not by a certificate authority.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			k, err := peerKey(raw)
			if err != nil {
				return err
			}
			if !k.Equal(panelKey) {
				return errors.New("the panel's key isn't the one this node joined: not connecting")
			}
			return nil
		},
	})
	fail := func(err error) (*yamux.Session, string, error) {
		tc.Close()
		return nil, "", err
	}
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.HandshakeContext(ctx); err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(tc).Encode(hello{Join: joinSecret, Version: version}); err != nil {
		return fail(err)
	}
	rd := bufio.NewReader(tc)
	var wel welcome
	if err := readLine(rd, &wel); err != nil {
		return fail(fmt.Errorf("the panel didn't answer: %w", err))
	}
	if wel.Error != "" {
		return fail(fmt.Errorf("the panel refused this node: %s", wel.Error))
	}
	_ = tc.SetDeadline(time.Time{})
	sess, err := yamux.Client(bufferedConn{tc, rd}, yamuxConfig())
	if err != nil {
		return fail(err)
	}
	return sess, wel.Version, nil
}

// readLine reads one JSON message, the line json.Encoder wrote, taking no
// more of what follows than rd buffers (yamux's frames are read from rd).
func readLine(rd *bufio.Reader, v any) error {
	line, err := rd.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// bufferedConn reads first what a reader of the hello took ahead.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 30 * time.Second // under Cloudflare's 100-second idle timeout
	c.LogOutput = nil
	c.Logger = discard{}
	return c
}

type discard struct{}

func (discard) Print(...any)          {}
func (discard) Printf(string, ...any) {}
func (discard) Println(...any)        {}
