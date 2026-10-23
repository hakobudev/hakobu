package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrLocked is a deletion the bucket's object lock refused: the object is
// younger than its rule keeps (R2's ObjectLockedByBucketPolicy).
var ErrLocked = errors.New("locked by the bucket's object lock")

// New is a client of bucket at endpoint (https://…, path-style), signed
// with the key; region "" is "auto", as R2 wants.
func New(endpoint, region, bucket, accessKeyID, secretAccessKey string) *Client {
	if region == "" {
		region = "auto"
	}
	return &Client{endpoint: endpoint, region: region, bucket: bucket, accessKeyID: accessKeyID, secretAccessKey: secretAccessKey}
}

func (c *Client) objectPath(key string) string { return "/" + c.bucket + "/" + escapePath(key) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Response, error) {
	req, err := c.signedRequest(method, path, query, body)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", method, path, err)
	}
	return resp, nil
}

func (c *Client) check(resp *http.Response, method, path string, ok ...int) error {
	for _, s := range ok {
		if resp.StatusCode == s {
			return nil
		}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	err := &statusError{resp.StatusCode, fmt.Sprintf("s3 %s %s failed (%d): %s", method, path, resp.StatusCode, msg)}
	if bytes.Contains(msg, []byte("<Code>ObjectLocked")) {
		return fmt.Errorf("%w: %w", ErrLocked, err)
	}
	return err
}

// Put writes an object.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	p := c.objectPath(key)
	resp, err := c.do(ctx, http.MethodPut, p, nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.check(resp, "PUT", p, http.StatusOK)
}

// Get reads an object; the caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p := c.objectPath(key)
	resp, err := c.do(ctx, http.MethodGet, p, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := c.check(resp, "GET", p, http.StatusOK); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// Delete removes an object; a missing one is no error.
func (c *Client) Delete(ctx context.Context, key string) error {
	p := c.objectPath(key)
	resp, err := c.do(ctx, http.MethodDelete, p, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.check(resp, "DELETE", p, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
}

// List names every object under prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		p := "/" + c.bucket
		resp, err := c.do(ctx, http.MethodGet, p, q, nil)
		if err != nil {
			return nil, err
		}
		if err := c.check(resp, "GET", p, http.StatusOK); err != nil {
			resp.Body.Close()
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, o.Key)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

// CreateBucket makes the client's bucket; one that's there already is no
// error.
func (c *Client) CreateBucket(ctx context.Context) error {
	p := "/" + c.bucket
	resp, err := c.do(ctx, http.MethodPut, p, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.check(resp, "PUT", p, http.StatusOK, http.StatusConflict)
}
