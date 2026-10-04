package cloudflare

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// R2 through the REST API with hakobu's API token. A single upload is
// capped at 300 MB; callers split larger files. A node uploads backups and
// downloads them through presigned URLs the panel makes with the token's
// S3 keys (R2Credentials), which never leave the panel.
const MaxObjectSize = 300 << 20

func r2Path(accountID, bucket string) string {
	return "/accounts/" + accountID + "/r2/buckets/" + bucket
}

func objectPath(accountID, bucket, key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return r2Path(accountID, bucket) + "/objects/" + strings.Join(segments, "/")
}

// r2Off is how Cloudflare refuses R2 calls in an account where R2 was
// never turned on, which only its owner can do, in the dashboard.
const r2Off = "enable R2 through the Cloudflare Dashboard"

// R2Off reports whether err is Cloudflare refusing because R2 isn't on in
// the account.
func R2Off(err error) bool { return err != nil && strings.Contains(err.Error(), r2Off) }

// R2URL is the account's R2 page in the dashboard, where R2 is turned on.
func R2URL(accountID string) string {
	return "https://dash.cloudflare.com/" + accountID + "/r2/overview"
}

// R2On asks whether R2 is on in the account, by listing a bucket.
func (c Client) R2On(accountID string) (bool, error) {
	err := c.call("GET", "/accounts/"+accountID+"/r2/buckets?per_page=1", nil, nil)
	if R2Off(err) {
		return false, nil
	}
	return err == nil, err
}

func (c Client) CreateBucket(accountID, name string) error {
	return c.call("POST", "/accounts/"+accountID+"/r2/buckets", map[string]string{"name": name}, nil)
}

// LockBucket stops anyone, hakobu included, from overwriting or deleting
// a file in the bucket for its first `days` days.
func (c Client) LockBucket(accountID, bucket string, days int) error {
	return c.call("PUT", r2Path(accountID, bucket)+"/lock", map[string]any{"rules": []map[string]any{{
		"id":        fmt.Sprintf("hakobu: keep backups %d days", days),
		"prefix":    "",
		"enabled":   true,
		"condition": map[string]any{"type": "Age", "maxAgeSeconds": days * 24 * 3600},
	}}}, nil)
}

// PutObject uploads size bytes from body, at most MaxObjectSize.
func (c Client) PutObject(accountID, bucket, key string, body io.Reader, size int64) error {
	if size > MaxObjectSize {
		return fmt.Errorf("r2: %s is %d bytes, over the %d byte limit", key, size, MaxObjectSize)
	}
	path := objectPath(accountID, bucket, key)
	resp, err := c.do("PUT", path, "application/octet-stream", body, size)
	if err != nil {
		return err
	}
	return c.decode("PUT", path, resp, nil)
}

// GetObject returns the object's body; the caller closes it.
func (c Client) GetObject(accountID, bucket, key string) (io.ReadCloser, error) {
	path := objectPath(accountID, bucket, key)
	resp, err := c.do("GET", path, "application/json", nil, -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.decode("GET", path, resp, nil)
	}
	return resp.Body, nil
}

func (c Client) DeleteObject(accountID, bucket, key string) error {
	return c.call("DELETE", objectPath(accountID, bucket, key), nil, nil)
}

// ListObjects returns the keys of every object whose key starts with
// prefix, in lexicographic order.
func (c Client) ListObjects(accountID, bucket, prefix string) ([]string, error) {
	var keys []string
	cursor := ""
	for {
		q := url.Values{"prefix": {prefix}, "per_page": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		path := r2Path(accountID, bucket) + "/objects?" + q.Encode()
		resp, err := c.do("GET", path, "application/json", nil, -1)
		if err != nil {
			return nil, err
		}
		var page struct {
			Result []struct {
				Key string `json:"key"`
			} `json:"result"`
			ResultInfo struct {
				Cursor      string `json:"cursor"`
				IsTruncated bool   `json:"is_truncated"`
			} `json:"result_info"`
		}
		raw, err := c.readEnvelope("GET", path, resp)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for _, o := range page.Result {
			keys = append(keys, o.Key)
		}
		if !page.ResultInfo.IsTruncated || page.ResultInfo.Cursor == "" || len(page.Result) == 0 {
			return keys, nil
		}
		cursor = page.ResultInfo.Cursor
	}
}

// R2Credentials are the S3 keys of the token for R2's S3 API: the token's
// ID and the SHA-256 of its value. They reach what the token does, so they
// stay with whoever holds the token; others get presigned URLs.
func (c Client) R2Credentials() (accessKeyID, secretAccessKey string, err error) {
	id, err := c.TokenID()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(c.Token))
	return id, hex.EncodeToString(sum[:]), nil
}

// TokenID is the ID of the client's token: an account's token is known to
// its account, a user's to the user.
func (c Client) TokenID() (string, error) {
	paths := []string{"/user/tokens/verify"}
	if c.AccountID != "" {
		paths = append([]string{"/accounts/" + c.AccountID + "/tokens/verify"}, paths...)
	}
	var errs []error
	for _, path := range paths {
		var t struct {
			ID string `json:"id"`
		}
		err := c.call("GET", path, nil, &t)
		if err == nil && t.ID != "" {
			return t.ID, nil
		}
		errs = append(errs, err)
	}
	return "", fmt.Errorf("finding the token's ID: %w", errors.Join(errs...))
}
