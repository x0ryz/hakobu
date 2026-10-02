// Package s3 is the little of S3 hakobu needs itself: checking which
// buckets a storage's keys reach. Apps talk to their storages with their
// own S3 clients.
package s3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
)

// defaultHTTP gives up on an endpoint that doesn't answer; the requests
// here are small.
var defaultHTTP = &http.Client{Timeout: time.Minute}

// Client signs requests with AWS Signature Version 4, path-style.
type Client struct {
	endpoint        string
	region          string
	bucket          string
	accessKeyID     string
	secretAccessKey string
	// HTTP sends the requests; nil is defaultHTTP.
	HTTP *http.Client
}

// R2Endpoint is R2's S3 endpoint for an account ID (%s); tests point it
// at a fake.
var R2Endpoint = "https://%s.r2.cloudflarestorage.com"

// Endpoint derives R2's endpoint from the account ID; other providers use
// the configured one.
func Endpoint(st store.Storage) string {
	if st.Provider == "r2" {
		return fmt.Sprintf(R2Endpoint, st.AccountID)
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

// CanList reports whether the client's keys may list bucket: false when
// the service refuses them (403), an error when it can't tell.
func (c *Client) CanList(bucket string) (bool, error) {
	err := c.call(http.MethodGet, "/"+bucket, url.Values{"list-type": {"2"}, "max-keys": {"1"}}, nil, http.StatusOK)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusForbidden {
		return false, nil
	}
	return err == nil, err
}

type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// call sends a signed request and checks its status.
func (c *Client) call(method, path string, query url.Values, body []byte, okStatus ...int) error {
	req, err := c.signedRequest(method, path, query, body)
	if err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
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
	return &statusError{resp.StatusCode, fmt.Sprintf("s3 %s %s failed (%d): %s", method, path, resp.StatusCode, msg)}
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
