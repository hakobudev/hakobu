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

	"github.com/x0ryz/hakobu/internal/store"
)

// Client is a minimal S3 client (SigV4, path-style) for whole-object
// PUT/GET; dumps are small enough to keep in memory.
type Client struct {
	endpoint        string
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
}

// Endpoint derives R2's endpoint from the account ID; other providers use
// the configured one.
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

func (c *Client) Endpoint() string { return c.endpoint }

// CreateBucket treats "already exists" (409) as success.
func (c *Client) CreateBucket() error {
	_, err := c.do(http.MethodPut, "", nil, "", http.StatusOK, http.StatusConflict)
	return err
}

func (c *Client) PutObject(key string, body []byte, contentType string) error {
	_, err := c.do(http.MethodPut, key, body, contentType, http.StatusOK)
	return err
}

func (c *Client) GetObject(key string) ([]byte, error) {
	return c.do(http.MethodGet, key, nil, "", http.StatusOK)
}

func (c *Client) do(method, key string, body []byte, contentType string, okStatus ...int) ([]byte, error) {
	req, err := c.signedRequest(method, key, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", method, key, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	for _, s := range okStatus {
		if resp.StatusCode == s {
			return respBody, nil
		}
	}
	return nil, fmt.Errorf("s3 %s %s failed (%d): %s", method, key, resp.StatusCode, respBody)
}

// signedRequest signs a request with AWS Signature Version 4.
func (c *Client) signedRequest(method, key string, body []byte) (*http.Request, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(c.endpoint, "https://"), "http://")
	canonicalURI := "/" + c.bucket
	if key != "" {
		segments := strings.Split(key, "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		canonicalURI += "/" + strings.Join(segments, "/")
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(body)

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, canonicalURI, "", canonicalHeaders, signedHeaders, payloadHash}, "\n")

	credentialScope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, credentialScope, sha256Hex([]byte(canonicalRequest))}, "\n")
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp), c.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req, err := http.NewRequest(method, c.endpoint+canonicalURI, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKeyID, credentialScope, signedHeaders, signature))
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
