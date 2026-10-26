// Package status tells a repository how its commit's deploy went, as a
// commit status next to its CI: pending while the server deploys it, then
// success or failure with the reason.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Context names hakobu's status among the commit's checks; CI waits for it.
const Context = "hakobu/deploy"

type State string

const (
	Pending State = "pending"
	Success State = "success"
	Failure State = "failure"
)

// Commit is the commit a release was built from, and the token that may
// set its status.
type Commit struct {
	Source   string // the repository's URL
	Revision string // the full SHA
	Token    string
}

// Reporter sets a commit's status.
type Reporter interface {
	Report(ctx context.Context, c Commit, state State, description string) error
}

// ErrNotGitHub: the source isn't a GitHub repository, so there's nowhere
// to report to.
var ErrNotGitHub = errors.New("not a GitHub repository")

// GitHub reports through GitHub's commit statuses, with the commit's token
// (a fine-grained token with "Commit statuses: write" on that repository
// alone is enough).
type GitHub struct {
	API  string // default https://api.github.com
	HTTP *http.Client
	// Since: Get skips statuses set before it, so a job run again for
	// the same commit waits for its own deploy, not the last run's.
	Since time.Time
}

func (g GitHub) Report(ctx context.Context, c Commit, state State, description string) error {
	repo, err := Repo(c.Source)
	if err != nil {
		return err
	}
	revision, token := c.Revision, c.Token
	if !fullSHA(revision) {
		return fmt.Errorf("%q isn't a commit's full SHA", revision)
	}
	if token == "" {
		return fmt.Errorf("no token to set commit statuses on %s", repo)
	}
	// GitHub keeps 140 characters of a description.
	if r := []rune(description); len(r) > 140 {
		description = string(r[:139]) + "…"
	}
	body, _ := json.Marshal(map[string]string{"state": string(state), "context": Context, "description": description})
	req, err := http.NewRequestWithContext(ctx, "POST", g.api()+"/repos/"+repo+"/statuses/"+revision, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := g.do(req, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("setting the status on %s@%s: %d %s", repo, revision[:7], resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Get reads the commit's hakobu/deploy status, as CI waits for it: ok is
// false while the server hasn't set one.
func (g GitHub) Get(ctx context.Context, c Commit) (state State, description string, ok bool, err error) {
	repo, err := Repo(c.Source)
	if err != nil {
		return "", "", false, err
	}
	// The release says which commit, and the token goes with the request.
	if !fullSHA(c.Revision) {
		return "", "", false, fmt.Errorf("%q isn't a commit's full SHA", c.Revision)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", g.api()+"/repos/"+repo+"/commits/"+c.Revision+"/statuses?per_page=100", nil)
	if err != nil {
		return "", "", false, err
	}
	resp, err := g.do(req, c.Token)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", false, fmt.Errorf("reading the statuses of %s@%s: %d %s", repo, c.Revision, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var statuses []struct {
		State       State     `json:"state"`
		Context     string    `json:"context"`
		Description string    `json:"description"`
		CreatedAt   time.Time `json:"created_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return "", "", false, err
	}
	// Newest first.
	for _, s := range statuses {
		if s.Context == Context && !s.CreatedAt.Before(g.Since) {
			return s.State, s.Description, true, nil
		}
	}
	return "", "", false, nil
}

func (g GitHub) api() string {
	if g.API != "" {
		return g.API
	}
	return "https://api.github.com"
}

func (g GitHub) do(req *http.Request, token string) (*http.Response, error) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return client.Do(req)
}

// Repo is "owner/name" of a GitHub repository's URL, as CI puts it in
// org.opencontainers.image.source.
func Repo(source string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(source), ".git"))
	if err != nil || u.Host != "github.com" {
		return "", ErrNotGitHub
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ErrNotGitHub
	}
	return parts[0] + "/" + parts[1], nil
}

func fullSHA(s string) bool {
	return len(s) == 40 && strings.Trim(s, "0123456789abcdef") == ""
}
