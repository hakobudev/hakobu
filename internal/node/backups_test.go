package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/x0ryz/hakobu/internal/secret"
)

// bucket is a store of objects by URL path that takes PUTs and GETs, as
// presigned URLs reach R2.
func bucket(t *testing.T) (objects map[string][]byte, urls func(key string) func(int) (string, error)) {
	objects = map[string][]byte{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case http.MethodGet:
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	return objects, func(key string) func(int) (string, error) {
		return func(part int) (string, error) {
			return fmt.Sprintf("%s/%s/%03d?X-Amz-Signature=x", srv.URL, key, part), nil
		}
	}
}

func TestBackupsGoUpSealedInParts(t *testing.T) {
	t.Chdir(t.TempDir()) // for tmpDir
	if err := secret.LoadKey(filepath.Join(t.TempDir(), "master.key")); err != nil {
		t.Fatal(err)
	}
	objects, urls := bucket(t)
	ctx := context.Background()
	dump := strings.Repeat("password=hunter2 ", 50)
	// Sealed with the node's own key, which the panel never has.
	up := Upload{PartSize: 100, URL: urls("main/1")}
	got, err := uploadSealed(ctx, up, func(w io.Writer) error {
		_, err := io.WriteString(w, dump)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parts != int((got.Size+99)/100) || got.Parts < 2 || len(objects) != got.Parts || got.KeyFingerprint != secret.Fingerprint() {
		t.Fatalf("uploaded %+v, objects %d", got, len(objects))
	}
	for k, v := range objects {
		if strings.Contains(string(v), "hunter2") {
			t.Errorf("%s holds the dump in the clear", k)
		}
	}
	dl := Download{KeyFingerprint: got.KeyFingerprint, SHA256: got.SHA256, Parts: got.Parts, URL: urls("main/1")}
	if body, err := download(ctx, dl); err != nil {
		t.Errorf("downloading an intact backup: %v", err)
	} else {
		b, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(b) != dump {
			t.Errorf("read back %d bytes, %v", len(b), err)
		}
	}

	// A backup changed in the bucket isn't restored.
	objects["/main/1/001"][0] ^= 1
	if body, err := download(ctx, dl); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("a tampered backup was downloaded: %v", err)
		if body != nil {
			body.Close()
		}
	}

	// Nothing unsealed is restored.
	objects["/old/1/000"] = []byte("plain dump")
	sum := sha256.Sum256([]byte("plain dump"))
	if body, err := download(ctx, Download{SHA256: hex.EncodeToString(sum[:]), Parts: 1, URL: urls("old/1")}); err == nil {
		body.Close()
		t.Error("an unsealed dump was read")
	}

	// A file the panel sends comes sealed with a key of its own.
	key, _ := secret.NewFileKey()
	sent, err := uploadSealed(ctx, Upload{FileKey: key, PartSize: 1 << 20, URL: urls("sent")}, func(w io.Writer) error {
		_, err := io.WriteString(w, "a file")
		return err
	})
	if err != nil || sent.KeyFingerprint != "" {
		t.Fatalf("sent %+v, %v", sent, err)
	}
	if body, err := download(ctx, Download{FileKey: key, SHA256: sent.SHA256, Parts: 1, URL: urls("sent")}); err != nil {
		t.Errorf("a file the panel sent: %v", err)
	} else {
		body.Close()
	}

	// A backup of another server's says which key to import.
	other := Download{KeyFingerprint: "0123456789abcdef", SHA256: sent.SHA256, Parts: 1, URL: urls("sent")}
	if body, err := download(ctx, other); err == nil || !strings.Contains(err.Error(), "hakobu key import") {
		t.Errorf("another server's backup: %v", err)
		if body != nil {
			body.Close()
		}
	}

	// An empty dump is a sealed file too, in one part.
	if got, err := uploadSealed(ctx, Upload{PartSize: 1 << 20, URL: urls("empty")}, func(io.Writer) error { return nil }); err != nil || got.Parts != 1 {
		t.Errorf("empty dump: %+v, %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(tmpDir, "*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	_ = os.RemoveAll(tmpDir)
}
