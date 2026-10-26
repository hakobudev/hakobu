package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestNewOwnerKey(t *testing.T) {
	f := filepath.Join(t.TempDir(), "owner-key")
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := newOwnerKey(f)
	w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 200)
	n, _ := r.Read(b)
	pub := strings.TrimSpace(string(b[:n]))
	key, _ := os.ReadFile(f)
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(key)))
	if err != nil || id.Recipient().String() != pub {
		t.Errorf("printed %q for %v", pub, err)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if err := newOwnerKey(f); err == nil {
		t.Error("made a second one over the first")
	}
}
