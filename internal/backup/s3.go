// Package backup implements database backups to Cloudflare R2 (or any
// S3-compatible store): a minimal hand-rolled S3 client (AWS Signature
// Version 4, path-style requests) rather than pulling in the full AWS SDK —
// hako only ever needs PutObject and GetObject, and every other
// dependency in this codebase talks to its target API directly (see
// internal/deploy's raw Docker socket client) rather than through an SDK.
package backup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hako/internal/store"
)

// Client talks to one S3-compatible bucket via path-style requests
// (https://<endpoint>/<bucket>/<key>).
type Client struct {
	endpoint        string
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
}

// Endpoint resolves a Storage's API host: R2 derives it from AccountID
// (Cloudflare's own <account_id>.r2.cloudflarestorage.com convention, so
// there's nothing to type in beyond the account ID); any other provider
// uses whatever endpoint was configured directly (AWS, MinIO, etc).
func Endpoint(st store.Storage) string {
	if st.Provider == "r2" {
		return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", st.AccountID)
	}
	return st.Endpoint
}

func NewClient(st store.Storage) *Client {
	region := st.Region
	if region == "" {
		region = "auto"
	}
	return &Client{
		endpoint:        Endpoint(st),
		region:          region,
		bucket:          st.Bucket,
		accessKeyID:     st.AccessKeyID,
		secretAccessKey: st.SecretAccessKey,
	}
}

// CreateBucket creates the client's bucket — only needed for a
// self-hosted target hako provisions itself (RustFS); an external R2
// or S3 bucket is assumed to already exist, created by the user in their
// provider's own console. A 409 (already owned by you) is treated as
// success, so this is safe to call unconditionally on every service
// startup.
func (c *Client) CreateBucket() error {
	req, err := c.signedRequest(http.MethodPut, "", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("bucket create failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bucket create failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// PutObject uploads body under key, entirely in memory — fine for the
// database-dump sizes a self-hosted side project produces; a multi-GB
// dump would need the multipart upload API instead.
func (c *Client) PutObject(key string, body []byte, contentType string) error {
	req, err := c.signedRequest(http.MethodPut, key, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("r2 put failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("r2 put failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// GetObject downloads key in full, same in-memory tradeoff as PutObject.
func (c *Client) GetObject(key string) ([]byte, error) {
	req, err := c.signedRequest(http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("r2 get failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("r2 get failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// signedRequest builds an AWS Signature Version 4 signed request for a
// single object PUT/GET — no chunked/streaming signing, since the whole
// body is already in memory and its exact hash can be computed up front.
func (c *Client) signedRequest(method, key string, body []byte) (*http.Request, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(c.endpoint, "https://"), "http://")
	canonicalURI := "/" + c.bucket
	if key != "" {
		canonicalURI += "/" + uriEncodePath(key)
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(body)

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		"", // no query string on these requests
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp), c.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKeyID, credentialScope, signedHeaders, signature,
	)

	reqURL := c.endpoint + canonicalURI
	req, err := http.NewRequest(method, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", authHeader)
	return req, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// uriEncodePath percent-encodes each path segment per SigV4's rules
// (RFC 3986 unreserved characters left alone, "/" preserved as a
// separator) — object keys here are generated by hako itself
// (timestamps, db names), but encoding correctly matters for the
// signature to match what R2 computes on its end.
func uriEncodePath(key string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
