package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/ci"
	"github.com/hakobudev/hakobu/internal/scaffold"
	srv "github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/status"
	"github.com/spf13/cobra"
)

// Setting a repository up, and what its CI runs.

func initCmd() *cobra.Command {
	var image, branch string
	c := &cobra.Command{
		Use:   "init",
		Short: "Set this repository up to deploy: hakobu.toml with the apps found, the CI workflow, and the skill and settings for Claude Code",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			server, _ := cmd.Flags().GetString("server")
			if server == "" {
				_, all, err := servers()
				if err != nil {
					return err
				}
				if names := all.Names(); len(names) == 1 {
					server = names[0]
				} else {
					return errors.New("which server? --server NAME (hakobu server list shows them)")
				}
			}
			repo := gitOut("remote", "get-url", "origin")
			if image == "" {
				r, err := status.Repo(strings.Replace(strings.Replace(repo, "git@github.com:", "https://github.com/", 1), "ssh://git@github.com/", "https://github.com/", 1))
				if err != nil {
					return errors.New("origin isn't on GitHub, so there's no ghcr.io image to guess: --image REGISTRY/NAME")
				}
				image = "ghcr.io/" + strings.ToLower(r)
			}
			if branch == "" {
				branch = strings.TrimPrefix(gitOut("symbolic-ref", "--short", "refs/remotes/origin/HEAD"), "origin/")
			}
			files, err := scaffold.Init(scaffold.Options{Dir: ".", Server: server, Image: image, Branch: branch, Version: cmd.Root().Version})
			for _, f := range files {
				if f.Kept {
					fmt.Printf("  kept   %s\n", f.Path)
				} else {
					fmt.Printf("  wrote  %s\n", f.Path)
				}
			}
			if err != nil {
				return err
			}
			// The server deploys the release from now on.
			release := image + ":main"
			_, _, registered := postSettings(cmd.Context(), server, "", srv.SettingsChange{AddRelease: release})
			fmt.Println()
			if registered != nil {
				fmt.Printf("%s doesn't know the release yet (%v): hakobu server set release %s %s\n", server, registered, server, release)
			} else {
				fmt.Printf("%s deploys %s from now on.\n", server, release)
			}
			fmt.Printf(`
Next:
  1. Read hakobu.toml: give each app its domain and health path, add databases.
  2. hakobu secret init, then a fine-grained GitHub token with "Commit
     statuses: write" on this repository alone: hakobu secret set hakobu status_token
  3. hakobu validate, commit, push: CI builds it and %s deploys it.
  4. The first time, while the job waits: on GitHub, the package %s
     (not its apps'), Package settings, Change visibility, Public. It's
     sealed; the server reads it with no token.
`, server, image[strings.LastIndex(image, "/")+1:])
			return nil
		},
	}
	c.Flags().StringVar(&image, "image", "", "the release image, without its tag (default: ghcr.io/OWNER/REPO from origin)")
	c.Flags().StringVar(&branch, "branch", "", "the branch whose pushes deploy (default: origin's default, or main)")
	return c
}

func gitOut(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func buildCmd() *cobra.Command {
	b := ci.Build{Dir: ".", Run: ci.Exec, Log: os.Stderr}
	c := &cobra.Command{
		Use:   "build",
		Short: "Build and push each app, then the release image its server polls (CI runs this; HAKOBU_PULL_TOKEN and HAKOBU_PULL_USER, CI's own, pull the apps)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if b.Revision == "" {
				b.Revision = gitOut("rev-parse", "HEAD")
			}
			if tok := os.Getenv("HAKOBU_PULL_TOKEN"); tok != "" {
				host, _, _ := strings.Cut(b.Image, "/")
				b.Pull = &apply.PullAuth{Host: host, Username: os.Getenv("HAKOBU_PULL_USER"), Password: tok}
			}
			ref, err := b.Release(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Println(ref)
			return nil
		},
	}
	c.Flags().StringVar(&b.Image, "image", "", "the release image, without its tag; app NAME goes to IMAGE-NAME")
	c.Flags().StringVar(&b.Tag, "tag", "main", "the tag the server polls")
	c.Flags().StringVar(&b.Source, "source", githubRepoURL(), "the repository's URL, for the commit status")
	c.Flags().StringVar(&b.Revision, "revision", os.Getenv("GITHUB_SHA"), "the commit (default: HEAD)")
	_ = c.MarkFlagRequired("image")
	return c
}

func waitCmd() *cobra.Command {
	var timeout time.Duration
	commit := status.Commit{}
	c := &cobra.Command{
		Use:   "wait",
		Short: "Wait for the server to deploy the commit and say how it went, from its hakobu/deploy status (CI runs this; the token is GITHUB_TOKEN)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if commit.Source == "" || commit.Revision == "" {
				return errors.New("which commit? --source and --revision (in GitHub Actions, they're found)")
			}
			commit.Token = os.Getenv("GITHUB_TOKEN")
			// The release was pushed just before: an older status is from a
			// run before this one (run again, the same commit has one). The
			// seconds are for GitHub's clock and the runner's.
			gh := status.GitHub{API: os.Getenv("GITHUB_API_URL"), Since: time.Now().Add(-10 * time.Second)}
			return ci.Wait(cmd.Context(), gh, commit, timeout, 10*time.Second, os.Stderr)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait")
	c.Flags().StringVar(&commit.Source, "source", githubRepoURL(), "the repository's URL")
	c.Flags().StringVar(&commit.Revision, "revision", os.Getenv("GITHUB_SHA"), "the commit")
	return c
}

// githubRepoURL is the repository's URL in GitHub Actions, "" elsewhere.
func githubRepoURL() string {
	if os.Getenv("GITHUB_REPOSITORY") == "" {
		return ""
	}
	server := os.Getenv("GITHUB_SERVER_URL")
	if server == "" {
		server = "https://github.com"
	}
	return server + "/" + os.Getenv("GITHUB_REPOSITORY")
}
