// Package tailscale puts the user's servers in their tailnet, so the apps
// there reach its devices by their 100.x addresses. hakobu works in the
// tailnet with an OAuth client the user creates for it: it never expires
// and does what its scopes allow, auth_keys to mint the one-off key a
// server joins with and devices:core to see the servers and remove them.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// APIURL is the API's base; tests point it at a fake.
var APIURL = "https://api.tailscale.com/api/v2"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ValidTag is a tag as Tailscale takes them.
var ValidTag = regexp.MustCompile(`^tag:[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// Client is an OAuth client of the user's tailnet; Tag is the one its keys
// carry, which the client must own.
type Client struct {
	ID, Secret, Tag string
}

// Device is a device of the tailnet as hakobu shows it.
type Device struct {
	NodeID    string   `json:"nodeId"`
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
	Tags      []string `json:"tags"`
	Online    bool     `json:"connectedToControl"`
	LastSeen  string   `json:"lastSeen"`
}

// IPv4 is the device's 100.x address.
func (d Device) IPv4() string {
	for _, a := range d.Addresses {
		if !strings.Contains(a, ":") {
			return a
		}
	}
	return ""
}

// tokens caches an access token per client for most of its hour.
var tokens = struct {
	sync.Mutex
	m map[string]token
}{m: map[string]token{}}

type token struct {
	value   string
	expires time.Time
}

func (c Client) token(ctx context.Context) (string, error) {
	tokens.Lock()
	defer tokens.Unlock()
	key := c.ID + "\x00" + c.Secret
	if t, ok := tokens.m[key]; ok && time.Until(t.expires) > 5*time.Minute {
		return t.value, nil
	}
	form := url.Values{"client_id": {c.ID}, "client_secret": {c.Secret}}
	req, err := http.NewRequestWithContext(ctx, "POST", APIURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := do(req, &out); err != nil {
		return "", fmt.Errorf("the OAuth client: %w", err)
	}
	tokens.m[key] = token{out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}
	return out.AccessToken, nil
}

func (c Client) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, APIURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(req, out)
}

func do(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return fmt.Errorf("Tailscale: %s (%d)", e.Message, resp.StatusCode)
		}
		return fmt.Errorf("Tailscale answered %d", resp.StatusCode)
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// AuthKey mints a key that puts one server in the tailnet with the
// client's tag, approved, for good (not ephemeral: the server stays while
// it's off). It works once, within ten minutes.
func (c Client) AuthKey(ctx context.Context, description string) (string, error) {
	body := map[string]any{
		"description":   description,
		"expirySeconds": 600,
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": false, "preauthorized": true, "tags": []string{c.Tag},
		}}},
	}
	var out struct{ Key string }
	if err := c.call(ctx, "POST", "/tailnet/-/keys", body, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Key, "tskey-auth-") {
		return "", errors.New("Tailscale returned no auth key")
	}
	return out.Key, nil
}

// Devices are the tailnet's devices.
func (c Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct{ Devices []Device }
	err := c.call(ctx, "GET", "/tailnet/-/devices", nil, &out)
	return out.Devices, err
}

// DeleteDevice removes a device from the tailnet; one already gone is fine.
func (c Client) DeleteDevice(ctx context.Context, nodeID string) error {
	err := c.call(ctx, "DELETE", "/device/"+url.PathEscape(nodeID), nil, nil)
	if err != nil && strings.Contains(err.Error(), "(404)") {
		return nil
	}
	return err
}

// Check makes sure the client does what hakobu needs: list devices, and
// mint keys with its tag (one is minted, and expires unused).
func (c Client) Check(ctx context.Context) error {
	if !ValidTag.MatchString(c.Tag) {
		return fmt.Errorf("%q isn't a tag: one like tag:hakobu", c.Tag)
	}
	if _, err := c.Devices(ctx); err != nil {
		return fmt.Errorf("listing devices (scope devices:core): %w", err)
	}
	if _, err := c.AuthKey(ctx, "hakobu check"); err != nil {
		return fmt.Errorf("minting a key with %s (scope auth_keys, and the client must have the tag): %w", c.Tag, err)
	}
	return nil
}
