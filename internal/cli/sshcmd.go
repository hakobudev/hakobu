package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/spf13/cobra"
)

// The commands an SSH key for hakobu may run, after ssh USER@SERVER: they
// talk to the server's API on its socket, as the operator token would.
// No shell, no files, nothing of root's: restore, destroy and the
// backups' buckets are root's (ssh root@SERVER hakobu …).
var sshCommands = [][]string{
	{"mcp"},
	{"status"}, {"deploys"}, {"logs"}, {"deploy"}, {"rollback"},
	{"backup"}, {"backups"},
	{"secret", "set"},
	{"server", "get"}, {"server", "set"}, {"server", "update"},
	{"--version"},
}

// sshHelp is what a key that runs hakobu's commands may run.
var sshHelp = [][2]string{
	{"status", "what runs, what's wrong, how full it is"},
	{"deploys", "the last deploys"},
	{"logs PROJECT/APP", "an app's output"},
	{"deploy PROJECT", "check for its release now"},
	{"rollback PROJECT", "back one release"},
	{"backup · backups PROJECT", "back up now · the backups"},
	{"secret set PROJECT APP NAME", "with ssh -t: seal a secret for the agent"},
	{"server get · set · update", "this server's settings, hakobu's updates"},
	{"mcp", "the MCP server, for an agent"},
}

func printSSHHelp(w io.Writer, name string) {
	l := lookFor(w)
	fmt.Fprintln(w, l.bold(name)+"  "+l.dim("this key runs hakobu's commands, and nothing else"))
	fmt.Fprintln(w)
	cmds := make([]string, len(sshHelp))
	for i, c := range sshHelp {
		cmds[i] = c[0]
	}
	cw := width(cmds...)
	for _, c := range sshHelp {
		// The command's words bold, its ARGUMENTS as they are.
		var words []string
		for _, f := range strings.Fields(c[0]) {
			switch {
			case f == "·":
				words = append(words, l.dim(f))
			case f == strings.ToUpper(f):
				words = append(words, f)
			default:
				words = append(words, l.bold(f))
			}
		}
		fmt.Fprintf(w, "  %s  %s\n", pad(strings.Join(words, " "), c[0], cw), l.dim(c[1]))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, l.dim("ssh "+name+" COMMAND. Restore, destroy and the backups' buckets are root's: ssh root@"+name+" hakobu …"))
}

func sshCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:    "ssh",
		Short:  "Run the command an SSH key for hakobu was given (sshd's forced command; SSH_ORIGINAL_COMMAND)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			overSSH = true
			return runSSH(cmd.Context(), version, os.Getenv("SSH_ORIGINAL_COMMAND"), cmd.OutOrStdout())
		},
	}
}

func runSSH(ctx context.Context, version, line string, out io.Writer) error {
	words, err := splitWords(line)
	if err != nil {
		return err
	}
	if len(words) == 0 || len(words) == 1 && slices.Contains([]string{"help", "--help", "-h"}, words[0]) {
		name := "this server"
		if all, err := known(); err == nil && len(all) == 1 {
			name = all.Names()[0]
		}
		printSSHHelp(out, name)
		return nil
	}
	if !sshAllowed(words) {
		return fmt.Errorf("this key runs hakobu's commands alone, and %q isn't one of them (ssh … help)", strings.Join(words, " "))
	}
	if len(words) >= 3 && words[0] == "server" && words[1] == "set" && words[2] == "backups" {
		return errors.New("the backups' buckets are root's: ssh root@… hakobu server set backups …")
	}
	if len(words) >= 2 && words[0] == "secret" && words[1] == "set" {
		if len(words) != 5 {
			return errors.New("secret set PROJECT APP NAME (ssh -t, to type the value unseen)")
		}
		return sendSecret(ctx, words[2], words[3], words[4], out)
	}
	root := Root(version)
	root.SetArgs(words)
	root.SetOut(out)
	return root.ExecuteContext(ctx)
}

func sshAllowed(words []string) bool {
	return slices.ContainsFunc(sshCommands, func(c []string) bool {
		return len(words) >= len(c) && slices.Equal(words[:len(c)], c)
	})
}

// sendSecret seals a value typed here to the server's key and its
// owner's, and hands it to the server, which keeps it for the agent.
func sendSecret(ctx context.Context, project, app, name string, out io.Writer) error {
	all, err := known()
	if err != nil {
		return err
	}
	if len(all) != 1 {
		return errors.New("no hakobu here (is it running?)")
	}
	c, err := all.Client(all.Names()[0])
	if err != nil {
		return err
	}
	var key map[string]string
	if err := c.Get(ctx, "/v1/key", &key); err != nil {
		return err
	}
	s, err := seal.New(key["recipient"], key["owner"])
	if err != nil {
		return err
	}
	v, err := readSecret(app + " " + name + ": ")
	if err != nil {
		return err
	}
	if v == "" {
		return errors.New("empty value")
	}
	if err := s.Set(project, app, name, v); err != nil {
		return err
	}
	sealed := s.Hakobu[name]
	if app != "hakobu" {
		sealed = s.Apps[app][name]
	}
	body := map[string]string{"project": project, "app": app, "name": name, "sealed": sealed}
	if err := c.Post(ctx, "/v1/secrets", body, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "Sealed: %s's %s, kept on %s for 7 days. Tell the agent to add it to hakobu.secrets.toml and push.\n", app, name, c.Name)
	return nil
}

// splitWords splits a command line as a shell would its words, quotes
// and backslashes included, and does nothing else a shell does: the
// words go to hakobu as they are.
func splitWords(s string) ([]string, error) {
	var words []string
	var w strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			w.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, in = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				w.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		default:
			w.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("the command's quotes aren't closed")
	}
	if in {
		words = append(words, w.String())
	}
	return words, nil
}
