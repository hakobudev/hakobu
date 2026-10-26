// Package client is the CLI's side of the servers' API: which servers
// there are (servers.toml on the laptop) and calls to them.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Server is one server as the laptop knows it. The token is the operator
// one: the admin token is never kept, it's given for the one call.
type Server struct {
	// URL is the API's, on the tailnet; or, with SSH, as the server sees
	// it (http://127.0.0.1:7070).
	URL   string `toml:"url"`
	Token string `toml:"token"`
	// SSH (user@host) and SSHKey, for an API on loopback: each connection
	// goes through ssh -W with that key, which may only reach the API.
	SSH    string `toml:"ssh,omitempty"`
	SSHKey string `toml:"ssh_key,omitempty"`
	// Socket is the API's Unix socket, on the server itself: no token,
	// the server knows who connects.
	Socket string `toml:"-"`
}

// Servers are the laptop's servers by name.
type Servers map[string]Server

// ServersFile is where they're kept.
func ServersFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hakobu", "servers.toml"), nil
}

func LoadServers(file string) (Servers, error) {
	var s struct {
		Servers Servers `toml:"servers"`
	}
	_, err := toml.DecodeFile(file, &s)
	if errors.Is(err, os.ErrNotExist) {
		return Servers{}, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Servers == nil {
		s.Servers = Servers{}
	}
	return s.Servers, nil
}

// Save writes the file readable by its owner alone: it holds tokens.
func (s Servers) Save(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]Servers{"servers": s}); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Names are the servers' names, sorted.
func (s Servers) Names() []string {
	var out []string
	for n := range s {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Client calls one server's API.
type Client struct {
	Name   string
	URL    string
	Token  string
	HTTP   *http.Client
	ssh    *sshTransport
	socket string
}

// Local says whether the client is on the server itself, on its socket.
func (c *Client) Local() bool { return c.socket != "" }

// Local is the server this machine is, through its socket, if there's
// one: the name is the server's.
func Local(socket string) (string, Server, bool) {
	if _, err := os.Stat(socket); err != nil {
		return "", Server{}, false
	}
	sv := Server{Socket: socket}
	c, _ := Servers{"": sv}.Client("")
	var key struct {
		Server string `json:"server"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Get(ctx, "/v1/key", &key); err != nil || key.Server == "" {
		return "", Server{}, false
	}
	return key.Server, sv, true
}

func (s Servers) Client(name string) (*Client, error) {
	sv, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("no server %s (hakobu server add)", name)
	}
	c := &Client{Name: name, URL: strings.TrimSuffix(sv.URL, "/"), Token: sv.Token}
	if sv.Socket != "" {
		c.URL, c.socket = "http://hakobu", sv.Socket
		return c, nil
	}
	if sv.SSH != "" {
		u, err := url.Parse(c.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("server %s: its url %q isn't one", name, sv.URL)
		}
		c.ssh = transportFor(sv.SSH, sv.SSHKey, u.Host)
	}
	return c, nil
}

// WithToken is the client calling with another token: the admin one.
func (c *Client) WithToken(token string) *Client {
	cc := *c
	cc.Token = token
	return &cc
}

// Get calls GET path and decodes the answer into v.
func (c *Client) Get(ctx context.Context, path string, v any) error {
	return c.Do(ctx, "GET", path, nil, v)
}

// Post calls POST path with body as JSON.
func (c *Client) Post(ctx context.Context, path string, body, v any) error {
	return c.Do(ctx, "POST", path, body, v)
}

func (c *Client) Do(ctx context.Context, method, path string, body, v any) error {
	resp, err := c.send(ctx, method, path, body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(b, v)
}

// Stream calls POST path asking for a stream (accept), and hands each line
// of the answer to line as it comes.
func (c *Client) Stream(ctx context.Context, path string, body any, accept string, line func([]byte) error) error {
	resp, err := c.send(ctx, "POST", path, body, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		if err := line(sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	return nil
}

// send makes the request, and turns an answer other than 200 into its
// error.
func (c *Client) send(ctx context.Context, method, path string, body any, accept string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	hc := c.HTTP
	if hc == nil {
		// Backups and deploys take a while; status doesn't.
		hc = &http.Client{Timeout: 3 * time.Hour}
		switch {
		case c.ssh != nil:
			hc.Transport = c.ssh.http
		case c.socket != "":
			sock := c.socket
			hc.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}}
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if c.ssh != nil {
			return nil, fmt.Errorf("%s: %w%s", c.Name, err, c.ssh.why())
		}
		if c.socket != "" {
			return nil, fmt.Errorf("%s: %w (is hakobu running? systemctl status hakobu)", c.Name, err)
		}
		return nil, fmt.Errorf("%s: %w (is this machine in the tailnet?)", c.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s: %s", c.Name, e.Error)
		}
		return nil, fmt.Errorf("%s: %s %s: %d", c.Name, method, path, resp.StatusCode)
	}
	return resp, nil
}
