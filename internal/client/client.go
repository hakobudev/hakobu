// Package client is the CLI's side of the servers' API: which servers
// there are (servers.toml on the laptop) and calls to them.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	URL   string `toml:"url"`
	Token string `toml:"token"`
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
	Name  string
	URL   string
	Token string
	HTTP  *http.Client
}

func (s Servers) Client(name string) (*Client, error) {
	sv, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("no server %s (hakobu server add)", name)
	}
	return &Client{Name: name, URL: strings.TrimSuffix(sv.URL, "/"), Token: sv.Token}, nil
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
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		// Backups and deploys take a while; status doesn't.
		hc = &http.Client{Timeout: 3 * time.Hour}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w (is this machine in the tailnet?)", c.Name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", c.Name, e.Error)
		}
		return fmt.Errorf("%s: %s %s: %d", c.Name, method, path, resp.StatusCode)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(b, v)
}
