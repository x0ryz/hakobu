// Package s3 is the little of S3 hakobu needs itself: creating the bucket of
// a RustFS storage and a RustFS user that can reach only that bucket. Apps
// talk to their storages with their own S3 clients.
package s3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
)

// Client signs requests with AWS Signature Version 4, path-style.
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
		secretAccessKey: string(st.SecretAccessKey),
	}
}

// CreateBucket treats "already exists" (409) as success.
func (c *Client) CreateBucket() error {
	return c.call(http.MethodPut, "/"+c.bucket, nil, nil, http.StatusOK, http.StatusConflict)
}

// RustFS's admin API (MinIO's, under /rustfs/admin/v3). The client must use
// the root keys.

const adminPrefix = "/rustfs/admin/v3/"

// AddBucketUser creates a user with its own keys and a policy of the same
// name allowing everything on the client's bucket and nothing else.
func (c *Client) AddBucketUser(accessKey, secretKey string) error {
	policy, _ := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":   "Allow",
			"Action":   []string{"s3:*"},
			"Resource": []string{"arn:aws:s3:::" + c.bucket, "arn:aws:s3:::" + c.bucket + "/*"},
		}},
	})
	user, _ := json.Marshal(map[string]string{"secretKey": secretKey, "status": "enabled"})
	for _, r := range []struct {
		path  string
		query url.Values
		body  []byte
	}{
		{"add-canned-policy", url.Values{"name": {accessKey}}, policy},
		{"add-user", url.Values{"accessKey": {accessKey}}, user},
		{"set-user-or-group-policy", url.Values{"policyName": {accessKey}, "userOrGroup": {accessKey}, "isGroup": {"false"}}, nil},
	} {
		if err := c.call(http.MethodPut, adminPrefix+r.path, r.query, r.body, http.StatusOK); err != nil {
			return err
		}
	}
	return nil
}

// RemoveBucketUser deletes a user from AddBucketUser and its policy; the
// bucket and its files stay. Ones already gone are fine.
func (c *Client) RemoveBucketUser(accessKey string) error {
	for _, r := range []struct {
		path  string
		query url.Values
	}{
		{"remove-user", url.Values{"accessKey": {accessKey}}},
		{"remove-canned-policy", url.Values{"name": {accessKey}}},
	} {
		// RustFS answers a missing user with a 500 that says so.
		err := c.call(http.MethodDelete, adminPrefix+r.path, r.query, nil, http.StatusOK, http.StatusNotFound)
		if err != nil && !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "not found") {
			return err
		}
	}
	return nil
}

// call sends a signed request and checks its status.
func (c *Client) call(method, path string, query url.Values, body []byte, okStatus ...int) error {
	req, err := c.signedRequest(method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	for _, s := range okStatus {
		if resp.StatusCode == s {
			return nil
		}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("s3 %s %s failed (%d): %s", method, path, resp.StatusCode, msg)
}

func (c *Client) signedRequest(method, path string, query url.Values, body []byte) (*http.Request, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(c.endpoint, "https://"), "http://")
	payloadHash := sha256Hex(body)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	q := canonicalQuery(query)

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, path, q, canonicalHeaders, signedHeaders, payloadHash}, "\n")

	credentialScope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, credentialScope, sha256Hex([]byte(canonicalRequest))}, "\n")
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp), c.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	rawURL := c.endpoint + path
	if q != "" {
		rawURL += "?" + q
	}
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKeyID, credentialScope, signedHeaders, signature))
	return req, nil
}

// canonicalQuery is SigV4's query string: sorted, every key with "=", and
// spaces as %20.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
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
