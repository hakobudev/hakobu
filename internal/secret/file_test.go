package secret

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var loadOnce sync.Once

// testKey loads one master key for the whole test binary, as a process
// uses one key.
func testKey(t *testing.T) string {
	t.Helper()
	loadOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hakobu-secret")
		if err != nil {
			t.Fatal(err)
		}
		keyPathForTests = filepath.Join(dir, "master.key")
		if err := LoadKey(keyPathForTests); err != nil {
			t.Fatal(err)
		}
	})
	return keyPathForTests
}

var keyPathForTests string

func seal(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewFileWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// Uneven writes, so chunk boundaries fall inside them.
	for p := plain; len(p) > 0; {
		n := min(len(p), 10_000)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func open(sealed []byte) ([]byte, error) {
	r, err := NewFileReader(bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSealedFileRoundtrip(t *testing.T) {
	testKey(t)
	for _, size := range []int{0, 1, fileChunk - 1, fileChunk, fileChunk + 1, 3*fileChunk + 5} {
		plain := make([]byte, size)
		rand.Read(plain)
		sealed := seal(t, plain)
		if bytes.Contains(sealed, plain[:min(size, 64)]) && size >= 16 {
			t.Errorf("size %d: plaintext visible in the sealed file", size)
		}
		got, err := open(sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("size %d: roundtrip failed (%v, %d bytes back)", size, err, len(got))
		}
	}
}

func TestSealedFileRejectsTampering(t *testing.T) {
	testKey(t)
	plain := make([]byte, 2*fileChunk+100)
	rand.Read(plain)
	sealed := seal(t, plain)
	header := len(fileMagic) + wrappedKeySize + noncePrefixSize
	chunk := fileChunk + 16

	cases := map[string][]byte{
		"cut at a chunk boundary": sealed[:header+2*chunk],
		"cut inside a chunk":      sealed[:header+chunk+500],
		"last chunk dropped":      sealed[:header+chunk],
		"header only":             sealed[:header],
	}
	flipped := bytes.Clone(sealed)
	flipped[header+chunk+7] ^= 1
	cases["a byte flipped"] = flipped
	swapped := bytes.Clone(sealed)
	copy(swapped[header:], sealed[header+chunk:header+2*chunk])
	copy(swapped[header+chunk:], sealed[header:header+chunk])
	cases["chunks swapped"] = swapped

	for name, bad := range cases {
		if _, err := open(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := open([]byte("plain gzip, not sealed")); err != errNotSealed {
		t.Errorf("unsealed file: %v, want errNotSealed", err)
	}
}

func TestSealedFileSurvivesRotation(t *testing.T) {
	keyPath := testKey(t)
	plain := []byte("pg_dump output")
	path := filepath.Join(t.TempDir(), "app.sql.gz.enc")
	if err := os.WriteFile(path, seal(t, plain), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.sql.gz.enc")
	if err := os.WriteFile(other, seal(t, plain), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	if err := BeginRotation(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := RewrapFile(path); err != nil {
		t.Fatal(err)
	}
	if err := FinishRotation(keyPath); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(path)
	header := len(fileMagic) + wrappedKeySize
	if !bytes.Equal(before[header:], after[header:]) || bytes.Equal(before[:header], after[:header]) {
		t.Error("rewrapping should change only the header")
	}
	if got, err := open(after); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("rewrapped file: %v %q", err, got)
	}
	// A file not rewrapped (a backup under the bucket's lock) still opens
	// with the retired key, kept in the keyring.
	stale, _ := os.ReadFile(other)
	if got, err := open(stale); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("a file sealed with the retired key: %v %q", err, got)
	}
	if _, err := os.Stat(KeyringDir(keyPath)); err != nil {
		t.Errorf("the retired key isn't in the keyring: %v", err)
	}
}

// A server's keys go out in one file and open its backups on another.
func TestExportImportKeys(t *testing.T) {
	testKey(t)
	plain := []byte("pg_dump output")
	sealed := seal(t, plain)
	file, err := ExportKeys()
	if err != nil {
		t.Fatal(err)
	}
	fps := Fingerprints()

	// Another server: its own key alone doesn't open the backup.
	mu.Lock()
	savedKeys, savedRing := keys, keyring
	keyring = nil
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); keys, keyring = savedKeys, savedRing; mu.Unlock() })
	other := filepath.Join(t.TempDir(), "master.key")
	mu.Lock()
	keys = nil
	mu.Unlock()
	if err := LoadKey(other); err != nil {
		t.Fatal(err)
	}
	if _, err := open(sealed); err == nil {
		t.Fatal("another server's key opened the backup")
	}
	added, err := ImportKeys(KeyringDir(other), file)
	if err != nil || len(added) != len(fps) {
		t.Fatalf("imported %v, %v; want %v", added, err, fps)
	}
	if got, err := open(sealed); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("after the import: %v %q", err, got)
	}
	if again, err := ImportKeys(KeyringDir(other), file); err != nil || len(again) != 0 {
		t.Errorf("importing again: %v, %v", again, err)
	}
	// A panel's master key file works too; a file without keys doesn't.
	if _, err := ImportKeys(KeyringDir(other), "# hakobu\nHAKOBU_BUCKET=b\n"); err == nil {
		t.Error("a file without keys imported")
	}
	panelKey := strings.TrimSpace(strings.Split(file, "\n")[1])
	if _, err := ImportKeys(KeyringDir(other), "HAKOBU_BUCKET=b\nHAKOBU_MASTER_KEY="+panelKey+"\n"); err != nil {
		t.Errorf("a panel's key file: %v", err)
	}
}

func TestSealedFileOwnKeyOutlivesRotation(t *testing.T) {
	keyPath := testKey(t)
	plain := []byte("a backup under the bucket's lock")
	var buf bytes.Buffer
	w, fileKey, err := NewFileWriterKey(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sealed := buf.Bytes()

	if err := BeginRotation(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := FinishRotation(keyPath); err != nil {
		t.Fatal(err)
	}
	// The retired key, kept in the keyring, opens the header too.
	if got, err := open(sealed); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("by its header: %v %q", err, got)
	}
	r, err := NewFileReaderKey(bytes.NewReader(sealed), fileKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("with its own key: %v %q", err, got)
	}
	_, wrongKey, _ := NewFileWriterKey(io.Discard)
	r, err = NewFileReaderKey(bytes.NewReader(sealed), wrongKey)
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Error("opened with another file's key")
	}
}

// A node seals a backup with a key the panel gives it, without a master
// key; only that key opens it.
func TestSealedFileWithGivenKey(t *testing.T) {
	testKey(t)
	key, err := NewFileKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, 2*fileChunk+3)
	rand.Read(plain)
	var buf bytes.Buffer
	w, err := NewFileWriterWithKey(&buf, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewFileReaderKey(bytes.NewReader(buf.Bytes()), key)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip failed (%v, %d bytes back)", err, len(got))
	}
	if _, err := open(buf.Bytes()); err == nil {
		t.Error("the master key opened a file sealed with a given key")
	}
	other, _ := NewFileKey()
	if r, err := NewFileReaderKey(bytes.NewReader(buf.Bytes()), other); err == nil {
		if _, err := io.ReadAll(r); err == nil {
			t.Error("another key opened it")
		}
	}
}
