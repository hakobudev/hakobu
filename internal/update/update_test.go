package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, current string
		want         bool
	}{
		{"v1.3.0", "v1.2.9", true},
		{"v1.10.0", "v1.9.0", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.1.9", "v1.2.0", false},
		{"v1.3.0", "dev", true},
		{"v1.3.0", "1.0.0-alpha.3", true}, // a pre-release is older than any release
		{"latest", "v1.2.0", false},
		{"v1.3.0-rc1", "v1.2.0", false},
		{"v1.3.0", "1.2.0", true}, // the binary's version comes without the v
		{"v1.3.0", "1.3.0", false},
	} {
		if got := Newer(c.tag, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.tag, c.current, got)
		}
	}
}

// fakeRelease serves releases signed with a key of the test's own.
type fakeRelease struct {
	priv   ed25519.PrivateKey
	files  map[string][]byte // "tag/file"
	latest string
}

func newFakeRelease(t *testing.T) *fakeRelease {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := publicKeys
	publicKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	f := &fakeRelease{priv: priv, files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/"+Repo+"/releases/latest" {
			fmt.Fprintf(w, `{"tag_name": %q}`, f.latest)
			return
		}
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/"+Repo+"/releases/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	oldAPI, oldDL := APIURL, DownloadURL
	APIURL, DownloadURL = srv.URL, srv.URL
	t.Cleanup(func() {
		srv.Close()
		publicKeys, APIURL, DownloadURL = old, oldAPI, oldDL
	})
	return f
}

// publish adds a signed release whose binary is body.
func (f *fakeRelease) publish(tag, body string) {
	sum := sha256.Sum256([]byte(body))
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + BinaryName() + "\n" + strings.Repeat("0", 64) + "  hakobu-linux-other\n")
	f.files[tag+"/"+BinaryName()] = []byte(body)
	f.files[tag+"/"+checksumsFile] = checksums
	f.files[tag+"/"+signatureFile] = ed25519.Sign(f.priv, SignedMessage(tag, checksums))
	f.latest = tag
}

func TestDownloadChecksSignatureAndChecksum(t *testing.T) {
	f := newFakeRelease(t)
	f.publish("v1.2.0", "old binary")
	f.publish("v1.3.0", "new binary")
	dst := filepath.Join(t.TempDir(), "hakobu.download")

	if err := Download("v1.3.0", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new binary" {
		t.Errorf("downloaded %q", b)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode())
	}

	good := map[string][]byte{}
	for k, v := range f.files {
		good[k] = v
	}
	for what, spoil := range map[string]func(){
		"a changed binary":  func() { f.files["v1.3.0/"+BinaryName()] = []byte("new binarY") },
		"changed checksums": func() { f.files["v1.3.0/"+checksumsFile] = append([]byte("x"), good["v1.3.0/"+checksumsFile]...) },
		"no signature":      func() { delete(f.files, "v1.3.0/"+signatureFile) },
		"another key's": func() {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			f.files["v1.3.0/"+signatureFile] = ed25519.Sign(other, SignedMessage("v1.3.0", good["v1.3.0/"+checksumsFile]))
		},
		"an older one renamed": func() {
			f.files["v1.3.0/"+checksumsFile], f.files["v1.3.0/"+signatureFile], f.files["v1.3.0/"+BinaryName()] = good["v1.2.0/"+checksumsFile], good["v1.2.0/"+signatureFile], good["v1.2.0/"+BinaryName()]
		},
		"no binary for this OS": func() { f.files["v1.3.0/"+checksumsFile] = []byte("abc  hakobu-linux-other\n") },
	} {
		for k, v := range good {
			f.files[k] = v
		}
		spoil()
		if err := Download("v1.3.0", dst); err == nil {
			t.Errorf("%s: installed", what)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the download was left behind", what)
		}
	}
}

// fakeServer is an install whose commands are played: the installer puts
// the downloaded binary in place, as install.sh does.
type fakeServer struct {
	in        *Install
	ran       []string
	installer string // what `server installer` prints
	broken    bool   // the installer fails
	up        func(string) bool
}

func newFakeServer(t *testing.T, version string) *fakeServer {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, binaryFile), []byte("binary "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{installer: "#!/usr/bin/env bash\n", up: func(string) bool { return true }}
	s.in = &Install{Dir: dir, Data: t.TempDir(), Version: version, Out: io.Discard}
	s.in.run = func(stream bool, name string, args ...string) (string, error) {
		s.ran = append(s.ran, strings.Join(append([]string{name}, args...), " "))
		switch {
		case slices.Equal(args, []string{"server", "installer"}):
			return s.installer, nil
		case name == "env":
			if s.broken {
				return "", errors.New("apt-get failed")
			}
			b, _ := os.ReadFile(filepath.Join(dir, downloadFile))
			return "", os.WriteFile(filepath.Join(dir, binaryFile), b, 0o755)
		}
		return "", nil
	}
	s.in.healthy = func(v string) bool {
		b, _ := os.ReadFile(filepath.Join(dir, binaryFile))
		return s.up(string(b)) && strings.HasSuffix(string(b), strings.TrimPrefix(v, "v"))
	}
	return s
}

func (s *fakeServer) binary() string {
	b, _ := os.ReadFile(filepath.Join(s.in.Dir, binaryFile))
	return string(b)
}

func (s *fakeServer) status(t *testing.T) Status {
	t.Helper()
	st, ok := ReadStatus(s.in.Dir)
	if !ok {
		t.Fatal("no status")
	}
	return st
}

func TestUpdate(t *testing.T) {
	f := newFakeRelease(t)
	f.publish("v1.3.0", "binary 1.3.0")
	s := newFakeServer(t, "1.2.0")
	if err := s.in.Update(""); err != nil {
		t.Fatal(err)
	}
	if s.binary() != "binary 1.3.0" {
		t.Errorf("binary %q", s.binary())
	}
	if b, _ := os.ReadFile(filepath.Join(s.in.Dir, prevFile)); string(b) != "binary 1.2.0" {
		t.Errorf("prev %q", b)
	}
	if st := s.status(t); st.State != "updated" || st.From != "v1.2.0" || st.To != "v1.3.0" {
		t.Errorf("status %+v", st)
	}
	for _, f := range []string{downloadFile, scriptFile} {
		if _, err := os.Stat(filepath.Join(s.in.Dir, f)); err == nil {
			t.Errorf("%s left", f)
		}
	}

	// Already the latest: nothing happens.
	s.in.Version = "1.3.0"
	s.ran = nil
	if err := s.in.Update(""); err != nil || len(s.ran) != 0 || s.status(t).State != "up to date" {
		t.Errorf("latest: %v, ran %v, %+v", err, s.ran, s.status(t))
	}
}

func TestUpdateRefusesOlder(t *testing.T) {
	f := newFakeRelease(t)
	f.publish("v1.1.0", "binary 1.1.0")
	f.publish("v1.2.0", "binary 1.2.0")
	s := newFakeServer(t, "1.2.0")
	for _, tag := range []string{"v1.1.0", "v1.2.0"} {
		if err := s.in.Update(tag); err == nil || s.binary() != "binary 1.2.0" || len(s.ran) != 0 {
			t.Errorf("%s: %v, %q, ran %v", tag, err, s.binary(), s.ran)
		}
	}
}

func TestUpdatePutsBack(t *testing.T) {
	for name, spoil := range map[string]func(*fakeServer){
		"doesn't come up": func(s *fakeServer) { s.up = func(b string) bool { return b != "binary 1.3.0" } },
		"installer fails": func(s *fakeServer) { s.broken = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRelease(t)
			f.publish("v1.3.0", "binary 1.3.0")
			s := newFakeServer(t, "1.2.0")
			spoil(s)
			err := s.in.Update("v1.3.0")
			if err == nil || s.binary() != "binary 1.2.0" {
				t.Errorf("%v, binary %q", err, s.binary())
			}
			if st := s.status(t); st.State != "rolled back" {
				t.Errorf("status %+v", st)
			}
			if !slices.Contains(s.ran, "systemctl restart hakobu") {
				t.Errorf("not restarted: %v", s.ran)
			}
		})
	}
}

func TestUpdateNeedsInstaller(t *testing.T) {
	f := newFakeRelease(t)
	f.publish("v1.3.0", "binary 1.3.0")
	s := newFakeServer(t, "1.2.0")
	s.installer = "unknown command"
	if err := s.in.Update(""); err == nil || s.binary() != "binary 1.2.0" || s.status(t).State != "failed" {
		t.Errorf("%v, %q, %+v", err, s.binary(), s.status(t))
	}
}

func TestReadRequest(t *testing.T) {
	data := t.TempDir()
	p := filepath.Join(data, RequestFile)
	for body, want := range map[string]string{"": "", "latest\n": "", "v1.3.0\n": "v1.3.0", "1.3.0": "v1.3.0"} {
		_ = os.WriteFile(p, []byte(body), 0o600)
		if got, err := ReadRequest(data); err != nil || got != want {
			t.Errorf("%q: %q, %v", body, got, err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%q: the request stays", body)
		}
	}

	secret := filepath.Join(t.TempDir(), "shadow")
	_ = os.WriteFile(secret, []byte("v9.9.9"), 0o600)
	_ = os.Symlink(secret, p)
	if _, err := ReadRequest(data); err == nil {
		t.Error("followed a symlink")
	}
	if _, err := os.Lstat(p); err == nil {
		t.Error("the symlink stays")
	}

	_ = os.WriteFile(p, []byte("root:$6$hash"), 0o600)
	if _, err := ReadRequest(data); err == nil || strings.Contains(err.Error(), "root") {
		t.Errorf("garbage: %v", err)
	}
}

func TestUpdateRequestedRefusedSays(t *testing.T) {
	s := newFakeServer(t, "1.2.0")
	_ = os.WriteFile(filepath.Join(s.in.Data, RequestFile), []byte("nonsense"), 0o600)
	if err := s.in.UpdateRequested(); err == nil || len(s.ran) != 0 || s.status(t).State != "failed" {
		t.Errorf("%v, ran %v, %+v", err, s.ran, s.status(t))
	}
}

func TestAllowed(t *testing.T) {
	for _, c := range []struct {
		policy, tag, current string
		want                 bool
	}{
		{AutoPatch, "v1.2.4", "1.2.3", true},
		{AutoPatch, "v1.3.0", "1.2.3", false},
		{AutoPatch, "v1.2.3", "1.2.3", false},
		{AutoMinor, "v1.3.0", "1.2.3", true},
		{AutoMinor, "v2.0.0", "1.2.3", false}, // a major one is always by hand
		{AutoNone, "v1.2.4", "1.2.3", false},
		{AutoPatch, "v1.2.4", "dev", false}, // a build from source updates when asked
		{AutoPatch, "v1.2.4", "1.2.4-rc.1", false},
	} {
		if got := Allowed(c.policy, c.tag, c.current); got != c.want {
			t.Errorf("Allowed(%s, %s, %s) = %v", c.policy, c.tag, c.current, got)
		}
	}
	for _, c := range []struct {
		tag, current string
		want         bool
	}{{"v1.3.0", "1.2.9", true}, {"v2.0.0", "1.9.9", true}, {"v1.2.9", "1.2.3", false}, {"v1.2.3", "1.3.0", false}, {"", "1.2.3", false}} {
		if got := Behind(c.tag, c.current); got != c.want {
			t.Errorf("Behind(%s, %s) = %v", c.tag, c.current, got)
		}
	}
}
