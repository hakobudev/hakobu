package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/secret"
)

// A server's backups and snapshots are sealed with its own keys, which never
// leave it through the panel: the owner exports them here, on the server,
// and imports them on a new one to restore its backups there.

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "This server's keys, which open its backups",
}

var keyExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Print this server's keys, to keep off it: without them its backups can't be restored",
	Long: `Print this server's keys, to keep off it: without them its backups can't be restored.
Run it in hakobu's directory:

  cd /opt/hakobu && sudo ./hakobu key export > ~/$(hostname).key`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := loadServerKeys(); err != nil {
			return err
		}
		file, err := secret.ExportKeys()
		if err != nil {
			return err
		}
		if _, err := io.WriteString(cmd.OutOrStdout(), file); err != nil {
			return err
		}
		// The panel stops asking for a copy of this key.
		if err := os.WriteFile(config.KeyExportedFile, []byte(secret.Fingerprint()+"\n"), 0o600); err != nil {
			return err
		}
		ownLike(config.KeyExportedFile, "data")
		fmt.Fprintln(cmd.ErrOrStderr(), "Keep this file away from the server: with it, a new server restores this one's backups (`hakobu key import`).")
		return nil
	},
}

var keyImportCmd = &cobra.Command{
	Use:   "import <file>",
	Short: "Add the keys of another server (from its `hakobu key export`), to restore its backups here",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var b []byte
		var err error
		if args[0] == "-" {
			b, err = io.ReadAll(cmd.InOrStdin())
		} else {
			b, err = os.ReadFile(args[0])
		}
		if err != nil {
			return err
		}
		if err := config.PrepareDataDir(); err != nil {
			return err
		}
		// A new server has no key yet: its own is made now.
		if err := secret.LoadKey(config.MasterKeyFile); err != nil {
			return err
		}
		dir := secret.KeyringDir(config.MasterKeyFile)
		if err := secret.LoadKeyring(dir); err != nil {
			return err
		}
		added, err := secret.ImportKeys(dir, string(b))
		if err != nil {
			return err
		}
		ownLike(dir, filepath.Dir(config.MasterKeyFile))
		for _, fp := range added {
			ownLike(filepath.Join(dir, fp+".key"), filepath.Dir(config.MasterKeyFile))
		}
		if len(added) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "This server has these keys already.")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Added %d key(s). Restart hakobu (systemctl restart hakobu) for it to use them.\n", len(added))
		return nil
	},
}

func init() {
	keyCmd.AddCommand(keyExportCmd, keyImportCmd)
	rootCmd.AddCommand(keyCmd)
}

// loadServerKeys loads the master key and keyring of the hakobu in the
// working directory; it never makes a key, as a new one would open nothing.
func loadServerKeys() error {
	if secret.KeyMissing(config.MasterKeyFile) {
		return errors.New("no hakobu key here: run this in hakobu's directory, e.g. cd /opt/hakobu && sudo ./hakobu key export")
	}
	if err := secret.LoadKey(config.MasterKeyFile); err != nil {
		return err
	}
	return secret.LoadKeyring(secret.KeyringDir(config.MasterKeyFile))
}

// ownLike gives path the owner of ref: run as root, the command mustn't
// leave files hakobu's own user can't read.
func ownLike(path, ref string) {
	info, err := os.Stat(ref)
	if err != nil {
		return
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
}
