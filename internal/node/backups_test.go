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
	objects, urls := bucket(t)
	ctx := context.Background()
	key, _ := secret.NewFileKey()
	dump := strings.Repeat("password=hunter2 ", 50)
	up := Upload{FileKey: key, PartSize: 100, URL: urls("main/1")}
	got, err := uploadSealed(ctx, up, func(w io.Writer) error {
		_, err := io.WriteString(w, dump)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parts != int((got.Size+99)/100) || got.Parts < 2 || len(objects) != got.Parts {
		t.Fatalf("uploaded %+v, objects %d", got, len(objects))
	}
	for k, v := range objects {
		if strings.Contains(string(v), "hunter2") {
			t.Errorf("%s holds the dump in the clear", k)
		}
	}
	dl := Download{FileKey: key, SHA256: got.SHA256, Parts: got.Parts, URL: urls("main/1")}
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

	// A dump from before backups were sealed is read as it is.
	objects["/old/1/000"] = []byte("plain dump")
	sum := sha256.Sum256([]byte("plain dump"))
	if body, err := download(ctx, Download{SHA256: hex.EncodeToString(sum[:]), Parts: 1, URL: urls("old/1")}); err != nil {
		t.Errorf("an unsealed dump: %v", err)
	} else {
		b, _ := io.ReadAll(body)
		body.Close()
		if string(b) != "plain dump" {
			t.Errorf("an unsealed dump read back %q", b)
		}
	}

	// An empty dump is a sealed file too, in one part.
	if got, err := uploadSealed(ctx, Upload{FileKey: key, PartSize: 1 << 20, URL: urls("empty")}, func(io.Writer) error { return nil }); err != nil || got.Parts != 1 {
		t.Errorf("empty dump: %+v, %v", got, err)
	}
	if _, err := uploadSealed(ctx, Upload{PartSize: 100, URL: urls("nokey")}, func(io.Writer) error { return nil }); err == nil {
		t.Error("uploaded a backup without a key to seal it with")
	}
	if left, _ := filepath.Glob(filepath.Join(tmpDir, "*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	_ = os.RemoveAll(tmpDir)
}
