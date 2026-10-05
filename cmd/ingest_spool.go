package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A node keeps the envelopes its apps send while the panel is out of
// reach (the link dropped, the panel restarts or its domain lapsed) in a
// spool on disk, and sends them on once it's linked again, oldest first.
// The spool holds the last day, 100 MB at most.
const (
	ingestSpoolDir      = "data/ingest-spool"
	ingestSpoolMaxBytes = 100 << 20
	ingestSpoolMaxAge   = 24 * time.Hour
)

// ingestHeaders are the request headers an envelope needs at the panel.
var ingestHeaders = []string{"Content-Type", "Content-Encoding", "X-Sentry-Auth"}

type ingestSpool struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	mu       sync.Mutex // over the directory
	draining sync.Mutex // one drain at a time
	seq      uint64     // orders envelopes of the same nanosecond
}

func newIngestSpool(dir string) *ingestSpool {
	return &ingestSpool{dir: dir, maxBytes: ingestSpoolMaxBytes, maxAge: ingestSpoolMaxAge}
}

// spooled is an envelope as kept: a JSON line, then the body as it came.
type spooled struct {
	URI    string
	Header map[string]string
}

// put keeps an envelope, dropping the oldest ones to stay within limits.
func (sp *ingestSpool) put(uri string, h http.Header, body []byte) error {
	meta := spooled{URI: uri, Header: map[string]string{}}
	for _, k := range ingestHeaders {
		if v := h.Get(k); v != "" {
			meta.Header[k] = v
		}
	}
	line, _ := json.Marshal(meta)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err := os.MkdirAll(sp.dir, 0o700); err != nil {
		return err
	}
	size := int64(len(line) + 1 + len(body))
	if size > sp.maxBytes {
		return fmt.Errorf("an envelope of %d bytes doesn't fit the spool", size)
	}
	sp.prune(size)
	sp.seq++
	name := fmt.Sprintf("%020d-%06d", time.Now().UnixNano(), sp.seq%1_000_000)
	tmp := filepath.Join(sp.dir, "."+name)
	if err := os.WriteFile(tmp, slices.Concat(line, []byte("\n"), body), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(sp.dir, name))
}

type spoolFile struct {
	name string
	size int64
	mod  time.Time
}

// files are the kept envelopes, oldest first.
func (sp *ingestSpool) files() []spoolFile {
	entries, _ := os.ReadDir(sp.dir)
	var out []spoolFile
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			out = append(out, spoolFile{e.Name(), fi.Size(), fi.ModTime()})
		}
	}
	return out // ReadDir sorts by name: by time
}

// prune drops envelopes past the age limit, then the oldest until room
// more bytes fit.
func (sp *ingestSpool) prune(room int64) {
	var total int64
	files := sp.files()
	for _, f := range files {
		total += f.size
	}
	for _, f := range files {
		if time.Since(f.mod) < sp.maxAge && total+room <= sp.maxBytes {
			break
		}
		if os.Remove(filepath.Join(sp.dir, f.name)) == nil {
			total -= f.size
		}
	}
}

// drain sends the kept envelopes to the panel through c, oldest first,
// until one doesn't get through; that one and the rest wait for the next
// drain. It returns how many it sent.
func (sp *ingestSpool) drain(c *http.Client) (int, error) {
	sp.draining.Lock()
	defer sp.draining.Unlock()
	sp.mu.Lock()
	sp.prune(0)
	files := sp.files()
	sp.mu.Unlock()
	sent := 0
	for _, f := range files {
		path := filepath.Join(sp.dir, f.name)
		retry, err := sendSpooled(c, path)
		if retry {
			return sent, err
		}
		if err == nil {
			sent++
		}
		// Sent, or refused for good (the app is gone, say): it goes.
		sp.mu.Lock()
		os.Remove(path)
		sp.mu.Unlock()
	}
	return sent, nil
}

// sendSpooled sends one kept envelope; retry tells it should be tried
// again later.
func sendSpooled(c *http.Client, path string) (retry bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, err // pruned meanwhile
	}
	if err != nil {
		return true, err
	}
	rd := bufio.NewReader(bytes.NewReader(b))
	line, err := rd.ReadBytes('\n')
	var meta spooled
	if err != nil || json.Unmarshal(line, &meta) != nil || !envelopePath.MatchString(strings.SplitN(meta.URI, "?", 2)[0]) {
		return false, fmt.Errorf("%s: not a kept envelope", path)
	}
	req, err := http.NewRequest(http.MethodPost, "http://panel"+meta.URI, rd)
	if err != nil {
		return false, err
	}
	for k, v := range meta.Header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("the panel answered %s", resp.Status)
	}
	return false, fmt.Errorf("the panel refused a kept envelope: %s", resp.Status)
}
