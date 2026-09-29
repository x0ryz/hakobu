package backup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
)

// partSize is the multipart chunk: files up to it go in one PUT, larger
// ones in parts of this size (S3 allows 10,000 parts, so up to 640 GB).
const partSize = 64 << 20

// Client is a minimal S3 client (SigV4, path-style). Objects are streamed
// from and to files, so a dump never has to fit in memory.
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

func (c *Client) Endpoint() string { return c.endpoint }

// CreateBucket treats "already exists" (409) as success.
func (c *Client) CreateBucket() error {
	_, err := c.call(http.MethodPut, "", nil, nil, "", http.StatusOK, http.StatusConflict)
	return err
}

// PutFile uploads a file, in parts if it's larger than partSize.
func (c *Client) PutFile(key string, f *os.File, contentType string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= partSize {
		_, err := c.call(http.MethodPut, key, nil, io.NewSectionReader(f, 0, info.Size()), contentType, http.StatusOK)
		return err
	}
	return c.putMultipart(key, f, info.Size(), contentType)
}

func (c *Client) putMultipart(key string, f *os.File, size int64, contentType string) error {
	body, err := c.call(http.MethodPost, key, url.Values{"uploads": {""}}, nil, contentType, http.StatusOK)
	if err != nil {
		return err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &created); err != nil || created.UploadID == "" {
		return fmt.Errorf("s3 multipart upload of %s: no upload ID in %q", key, body)
	}
	id := url.Values{"uploadId": {created.UploadID}}

	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []part
	for offset, n := int64(0), 1; offset < size; offset, n = offset+partSize, n+1 {
		q := url.Values{"uploadId": id["uploadId"], "partNumber": {fmt.Sprint(n)}}
		resp, err := c.do(http.MethodPut, key, q, io.NewSectionReader(f, offset, min(partSize, size-offset)), "")
		if err == nil {
			resp.Body.Close()
			parts = append(parts, part{n, resp.Header.Get("ETag")})
			continue
		}
		c.call(http.MethodDelete, key, id, nil, "", http.StatusNoContent)
		return err
	}
	done, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	body, err = c.call(http.MethodPost, key, id, bytes.NewReader(done), "application/xml", http.StatusOK)
	if err != nil {
		c.call(http.MethodDelete, key, id, nil, "", http.StatusNoContent)
		return err
	}
	// S3 reports some failures with 200 and an <Error> body.
	if bytes.Contains(body, []byte("<Error>")) {
		return fmt.Errorf("s3 multipart upload of %s failed: %s", key, body)
	}
	return nil
}

// GetStream returns the object's body; the caller closes it.
func (c *Client) GetStream(key string) (io.ReadCloser, error) {
	resp, err := c.do(http.MethodGet, key, nil, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) DeleteObject(key string) error {
	_, err := c.call(http.MethodDelete, key, nil, nil, "", http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	return err
}

// call is do for small responses: it reads the body and checks the status.
func (c *Client) call(method, key string, query url.Values, body io.ReadSeeker, contentType string, okStatus ...int) ([]byte, error) {
	resp, err := c.do(method, key, query, body, contentType, okStatus...)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// do sends a signed request and returns the response if its status is in
// okStatus (default 200).
func (c *Client) do(method, key string, query url.Values, body io.ReadSeeker, contentType string, okStatus ...int) (*http.Response, error) {
	req, err := c.signedRequest(method, key, query, body)
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
	if len(okStatus) == 0 {
		okStatus = []int{http.StatusOK}
	}
	for _, s := range okStatus {
		if resp.StatusCode == s {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return nil, fmt.Errorf("s3 %s %s failed (%d): %s", method, key, resp.StatusCode, msg)
}

// signedRequest signs a request with AWS Signature Version 4. The body is
// read once to hash it, then rewound to be sent.
func (c *Client) signedRequest(method, key string, query url.Values, body io.ReadSeeker) (*http.Request, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(c.endpoint, "https://"), "http://")
	canonicalURI := "/" + c.bucket
	if key != "" {
		segments := strings.Split(key, "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		canonicalURI += "/" + strings.Join(segments, "/")
	}

	h := sha256.New()
	var size int64
	if body != nil {
		var err error
		if size, err = io.Copy(h, body); err != nil {
			return nil, err
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, canonicalURI, canonicalQuery(query), canonicalHeaders, signedHeaders, payloadHash}, "\n")

	credentialScope := dateStamp + "/" + c.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, credentialScope, sha256Hex([]byte(canonicalRequest))}, "\n")
	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.secretAccessKey), dateStamp), c.region), "s3"), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	rawURL := c.endpoint + canonicalURI
	if q := canonicalQuery(query); q != "" {
		rawURL += "?" + q
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = body
	}
	req, err := http.NewRequest(method, rawURL, reqBody)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
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
			parts = append(parts, s3Escape(k)+"="+s3Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func s3Escape(s string) string {
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
