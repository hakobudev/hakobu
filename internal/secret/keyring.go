package secret

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// A server's keyring holds keys that only open what they sealed before: its
// own master keys from before a rotation, and those of servers whose backups
// it restores. Backups under the bucket's lock can't be rewrapped, so their
// keys are kept rather than forgotten. One file per key, named by its
// fingerprint, next to the master key.

// keyring is decrypt-only: the master key (keys[0]) still seals.
var keyring []key

// KeyringDir is the keyring next to the master key at masterPath.
func KeyringDir(masterPath string) string {
	return filepath.Join(filepath.Dir(masterPath), "keyring")
}

func fingerprintOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// LoadKeyring adds the keys in dir to those that open sealed values and
// files; a missing dir is an empty keyring.
func LoadKeyring(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".key") {
			continue
		}
		raw, err := readFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if err := addToKeyring(raw); err != nil {
			return err
		}
	}
	return nil
}

func addToKeyring(raw []byte) error {
	mu.Lock()
	defer mu.Unlock()
	for _, k := range append(keys, keyring...) {
		if string(k.raw) == string(raw) {
			return nil
		}
	}
	k, err := newKey(raw)
	if err != nil {
		return err
	}
	keyring = append(keyring, k)
	return nil
}

// saveToKeyring keeps raw in dir and opens with it from now on.
func saveToKeyring(dir string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeKey(filepath.Join(dir, fingerprintOf(raw)+".key"), raw); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return addToKeyring(raw)
}

// openingKeys are the keys that open: the master key(s), then the keyring.
func openingKeys() ([]key, error) {
	ks, err := loadedKeys()
	if err != nil {
		return nil, err
	}
	mu.RLock()
	defer mu.RUnlock()
	return append(append([]key{}, ks...), keyring...), nil
}

// Fingerprints names every key that opens, the master key first.
func Fingerprints() []string {
	ks, err := openingKeys()
	if err != nil {
		return nil
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = fingerprintOf(k.raw)
	}
	return out
}

// keysHeader starts a file from ExportKeys.
const keysHeader = "# hakobu server keys: they open this server's backups. Keep this file off the server.\n"

// ExportKeys is every key that opens, the master key first, as a file for
// the owner to keep: a line per key, base64.
func ExportKeys() (string, error) {
	ks, err := openingKeys()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(keysHeader)
	for _, k := range ks {
		b.WriteString(base64.StdEncoding.EncodeToString(k.raw) + "\n")
	}
	return b.String(), nil
}

// ImportKeys saves the keys of a file from ExportKeys (or a panel's master
// key file) into dir, returning the fingerprints of those it didn't have.
func ImportKeys(dir, file string) ([]string, error) {
	known := map[string]bool{}
	for _, fp := range Fingerprints() {
		known[fp] = true
	}
	var added []string
	found := 0
	for _, line := range strings.Split(file, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, envKey+"=")
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "=") && !strings.HasSuffix(line, "=") {
			continue // a comment, or another setting of a panel's key file
		}
		raw, err := decodeKey(line, "a key in the file")
		if err != nil {
			return added, err
		}
		found++
		fp := fingerprintOf(raw)
		if known[fp] {
			continue
		}
		if err := saveToKeyring(dir, raw); err != nil {
			return added, err
		}
		known[fp] = true
		added = append(added, fp)
	}
	if found == 0 {
		return nil, errors.New("no keys in the file")
	}
	return added, nil
}
