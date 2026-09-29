// Package s3 is the little of S3 hakobu needs itself: creating the bucket of
// a RustFS storage. Apps talk to their storages with their own S3 clients.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
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
	req, err := c.signedRequest(http.MethodPut)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 create bucket %s: %w", c.bucket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("s3 create bucket %s failed (%d): %s", c.bucket, resp.StatusCode, msg)
}

// signedRequest signs a bodyless request on the bucket.
func (c *Client) signedRequest(method string) (*http.Request, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(c.endpoint, "https://"), "http://")
	uri := "/" + c.bucket
	payloadHash := sha256Hex(nil)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, uri, "", canonicalHeaders, signedHeaders, payloadHash}, "\n")

	credentialScope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, credentialScope, sha256Hex([]byte(canonicalRequest))}, "\n")
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp), c.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req, err := http.NewRequest(method, c.endpoint+uri, nil)
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
