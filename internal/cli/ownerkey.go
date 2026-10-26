package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The owner's key, made on the server when the person has no age of
// their own: its secret half waits there, root's alone, until the person
// takes it into their password manager, at a terminal an agent isn't at.
func ownerKeyCmd() *cobra.Command {
	var newKey bool
	file := server.OwnerKeyFile
	c := &cobra.Command{
		Use:   "owner-key",
		Short: "Show the owner's key made here, for your password manager, and take it off the server (as root, at a terminal: ssh -t root@SERVER hakobu server owner-key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Geteuid() != 0 {
				return errors.New("the owner's key is root's: ssh -t root@SERVER hakobu server owner-key")
			}
			if newKey {
				return newOwnerKey(file)
			}
			b, err := os.ReadFile(file)
			if errors.Is(err, fs.ErrNotExist) {
				return errors.New("no owner's key here: it was taken already, or you gave your own at install")
			}
			if err != nil {
				return err
			}
			id, err := age.ParseX25519Identity(strings.TrimSpace(string(b)))
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			// Shown to a person, not to an agent's Bash, which has no
			// terminal.
			if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return errors.New("this shows the owner's secret key to a person at a terminal, and there's none: run ssh -t root@SERVER hakobu server owner-key in your own terminal")
			}
			fmt.Printf(`Your owner's key: every secret and backup of this server is sealed to it
too, so with it you can open them without the server. Keep it in your
password manager, and nowhere an agent reads.

  %s

public half: %s

`, id.String(), id.Recipient().String())
			if err := confirm("Saved it? Type saved to take it off this server: ", "saved"); err != nil {
				return err
			}
			if err := os.Remove(file); err != nil {
				return err
			}
			fmt.Println("Taken off the server. Only your copy is left.")
			return nil
		},
	}
	c.Flags().BoolVar(&newKey, "new", false, "make one and print its public half (install.sh does, without HAKOBU_OWNER_KEY)")
	_ = c.Flags().MarkHidden("new")
	return c
}

func newOwnerKey(file string) error {
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("%s is there already", file)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, id.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(id.Recipient().String())
	return nil
}
