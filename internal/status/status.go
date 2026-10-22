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
}

func (g GitHub) Report(ctx context.Context, c Commit, state State, description string) error {
	repo, err := Repo(c.Source)
	if err != nil {
		return err
	}
	revision, token := c.Revision, c.Token
	if len(revision) != 40 || strings.Trim(revision, "0123456789abcdef") != "" {
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
	api := g.API
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/repos/"+repo+"/statuses/"+revision, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
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
