package cloudflare

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// R2 through the REST API with the OAuth token, so no S3 keys exist that
// could leak. A single upload is capped at 300 MB; callers split larger
// files.
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
	return decode("PUT", path, resp, nil)
}

// GetObject returns the object's body; the caller closes it.
func (c Client) GetObject(accountID, bucket, key string) (io.ReadCloser, error) {
	path := objectPath(accountID, bucket, key)
	resp, err := c.do("GET", path, "application/json", nil, -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decode("GET", path, resp, nil)
	}
	return resp.Body, nil
}

func (c Client) DeleteObject(accountID, bucket, key string) error {
	return c.call("DELETE", objectPath(accountID, bucket, key), nil, nil)
}
