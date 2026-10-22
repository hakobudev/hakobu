// Package registry reads images from an OCI registry over its HTTP API,
// without Docker: the digest a tag points at (one HEAD a poll) and an
// image's labels, which is all a release image carries.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Ref is an image reference: host/repository, and a tag or a digest.
type Ref struct {
	Host, Repo string
	Tag        string // "" when pinned by Digest
	Digest     string // "sha256:…"
}

// ParseRef reads "ghcr.io/org/app:main", "ghcr.io/org/app@sha256:…" or
// Docker Hub's short forms ("busybox:1.36").
func ParseRef(s string) (Ref, error) {
	var r Ref
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		rest, r.Digest = rest[:i], rest[i+1:]
		if !strings.HasPrefix(r.Digest, "sha256:") || len(r.Digest) != len("sha256:")+64 {
			return r, fmt.Errorf("%q: the digest should be sha256: and 64 hex digits", s)
		}
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest, r.Tag = rest[:i], rest[i+1:]
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	first, after, found := strings.Cut(rest, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Host, r.Repo = first, after
	} else {
		r.Host, r.Repo = "registry-1.docker.io", rest
		if !found {
			r.Repo = "library/" + rest
		}
	}
	if r.Repo == "" || strings.ToLower(r.Repo) != r.Repo {
		return r, fmt.Errorf("%q isn't an image reference", s)
	}
	return r, nil
}

func (r Ref) String() string {
	s := r.Host + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Pinned is the ref pinned to digest, without its tag.
func (r Ref) Pinned(digest string) Ref {
	return Ref{Host: r.Host, Repo: r.Repo, Digest: digest}
}

func (r Ref) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Client talks to registries, signed in where it has credentials.
type Client struct {
	// Auth are the credentials of each host ("ghcr.io": a user and a
	// token that can read packages); others are read anonymously.
	Auth map[string]Credentials
	HTTP *http.Client
	// Insecure hosts speak plain HTTP (tests, a local registry).
	Insecure map[string]bool

	mu     sync.Mutex
	tokens map[string]string // host + scope → bearer token
}

type Credentials struct{ Username, Password string }

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Digest is what ref's tag points at now: one HEAD of its manifest, which
// registries don't count as a pull.
func (c *Client) Digest(ctx context.Context, r Ref) (string, error) {
	resp, err := c.do(ctx, "HEAD", r, "/manifests/"+r.reference(), manifestTypes)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("%s: the registry gave no digest", r)
	}
	return d, nil
}

// Labels are the labels of the image r names, for this machine's platform
// when r is an index of several.
func (c *Client) Labels(ctx context.Context, r Ref) (map[string]string, error) {
	var m struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := c.getJSON(ctx, r, "/manifests/"+r.reference(), manifestTypes, &m); err != nil {
		return nil, err
	}
	if len(m.Manifests) > 0 {
		pick := ""
		for _, d := range m.Manifests {
			// Attestations ride along as "unknown/unknown".
			if d.Platform.OS == "linux" && d.Platform.Architecture == runtime.GOARCH {
				pick = d.Digest
				break
			}
		}
		if pick == "" {
			for _, d := range m.Manifests {
				if d.Platform.OS != "unknown" {
					pick = d.Digest
					break
				}
			}
		}
		if pick == "" {
			return nil, fmt.Errorf("%s: no image in the index", r)
		}
		return c.Labels(ctx, r.Pinned(pick))
	}
	if m.Config.Digest == "" {
		return nil, fmt.Errorf("%s: not an image manifest", r)
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := c.getJSON(ctx, r, "/blobs/"+m.Config.Digest, "", &cfg); err != nil {
		return nil, err
	}
	return cfg.Config.Labels, nil
}

// maxDocument caps a manifest or config read: release labels are small.
const maxDocument = 4 << 20

func (c *Client) getJSON(ctx context.Context, r Ref, path, accept string, v any) error {
	resp, err := c.do(ctx, "GET", r, path, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s%s: %w", r, path, err)
	}
	return nil
}

// ErrNotFound: the registry has no such tag, digest or repository (or
// won't say it has, to someone not signed in).
var ErrNotFound = errors.New("not found")

// do sends a request for r's repository, signing in when the registry
// asks: a bearer token from its realm (with the host's credentials, or
// none), or basic auth.
func (c *Client) do(ctx context.Context, method string, r Ref, path, accept string) (*http.Response, error) {
	scope := "repository:" + r.Repo + ":pull"
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base(r.Host)+"/v2/"+r.Repo+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if tok := c.token(r.Host, scope); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if cr, ok := c.Auth[r.Host]; ok && attempt > 0 {
			req.SetBasicAuth(cr.Username, cr.Password)
		}
		resp, err := c.client().Do(req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if err := c.signIn(ctx, r.Host, scope, challenge); err != nil {
				return nil, fmt.Errorf("%s: signing in: %w", r, err)
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s: %w", r, ErrNotFound)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%s: the registry refused (%d): check the credentials for %s", r, resp.StatusCode, r.Host)
		}
		return nil, fmt.Errorf("%s: %s %s: %d %s", r, method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// signIn answers a 401's challenge. Basic needs nothing more: the retry
// sends the credentials. Bearer fetches a token for scope from the realm.
func (c *Client) signIn(ctx context.Context, host, scope, challenge string) error {
	kind, params := parseChallenge(challenge)
	switch kind {
	case "basic":
		if _, ok := c.Auth[host]; !ok {
			return fmt.Errorf("%s wants a user and password, and hakobu has none for it", host)
		}
		return nil
	case "bearer":
	default:
		return fmt.Errorf("unexpected challenge %q", challenge)
	}
	u, err := url.Parse(params["realm"])
	if err != nil || u.Host == "" {
		return fmt.Errorf("bad realm in %q", challenge)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	if cr, ok := c.Auth[host]; ok {
		req.SetBasicAuth(cr.Username, cr.Password)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the token service answered %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return err
	}
	tok := t.Token
	if tok == "" {
		tok = t.AccessToken
	}
	if tok == "" {
		return errors.New("the token service gave no token")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[host+" "+scope] = tok
	return nil
}

// token is a bearer token got before. Registries' tokens last minutes; a
// stale one gets a 401, which signs in again.
func (c *Client) token(host, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[host+" "+scope]
}

func (c *Client) base(host string) string {
	if c.Insecure[host] {
		return "http://" + host
	}
	return "https://" + host
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (kind string, params map[string]string) {
	kind, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params = map[string]string{}
	for rest != "" {
		var kv string
		rest = strings.TrimLeft(rest, ", ")
		if rest == "" {
			break
		}
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			kv, rest = after[1:end+1], after[end+2:]
		} else {
			kv, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(k))] = kv
	}
	return strings.ToLower(kind), params
}
