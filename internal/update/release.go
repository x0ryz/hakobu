// Package update installs hakobu's releases: it finds the latest one,
// downloads it, checks its signature, swaps the binary and puts the
// previous one back if the new one doesn't come up.
//
// A release is trusted only if it's signed: GitHub (or whoever gets at
// the repository) can serve any file, but only the holder of the signing
// key can make a signature one of publicKeys accepts. What's signed is
// "hakobu <tag>\n" followed by the release's checksums.txt, so the files of
// an older release can't be passed off as a newer one.
package update

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases come from.
const Repo = "x0ryz/hakobu"

// publicKeys verify release signatures (raw Ed25519 keys, base64). A new
// key is added here in a release signed with the old one, which goes once
// every server has updated past that release.
var publicKeys = []string{
	"WBkGQ8EE1VzJYgZkPLxDY1JCviWclccA/tDM/FnQ0fI=",
}

// Asset files of a release.
const (
	checksumsFile = "checksums.txt"
	signatureFile = "checksums.txt.sig"
)

// BinaryName is the release file for this machine.
func BinaryName() string { return "hakobu-linux-" + runtime.GOARCH }

var (
	APIURL      = "https://api.github.com"
	DownloadURL = "https://github.com"
	client      = &http.Client{Timeout: 10 * time.Minute}
)

// Tag writes a version the way release tags are, v1.2.3; the binary's own
// version comes without the v.
func Tag(version string) string {
	if _, ok := parseTag(version); ok {
		return "v" + strings.TrimPrefix(version, "v")
	}
	return version
}

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Newer reports whether tag is a later release than current; a current
// version that isn't a release (a build from source) is older than any.
func Newer(tag, current string) bool {
	t, ok := parseTag(tag)
	if !ok {
		return false
	}
	c, ok := parseTag(current)
	if !ok {
		return true
	}
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

func parseTag(tag string) ([3]int, bool) {
	var v [3]int
	m := tagPattern.FindStringSubmatch("v" + strings.TrimPrefix(tag, "v"))
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// Latest returns the tag of the latest release.
func Latest() (string, error) {
	req, err := http.NewRequest("GET", APIURL+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("asking GitHub for the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("asking GitHub for the latest release: %s", resp.Status)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", err
	}
	if _, ok := parseTag(r.TagName); !ok {
		return "", fmt.Errorf("the latest release has an odd tag %q", r.TagName)
	}
	return r.TagName, nil
}

// SignedMessage is what a release's signature covers.
func SignedMessage(tag string, checksums []byte) []byte {
	return append([]byte("hakobu "+tag+"\n"), checksums...)
}

// VerifySignature checks that sig is a signature of tag's checksums by
// one of publicKeys.
func VerifySignature(tag string, checksums, sig []byte) error {
	return verifyWith(publicKeys, tag, checksums, sig)
}

func verifyWith(keys []string, tag string, checksums, sig []byte) error {
	msg := SignedMessage(tag, checksums)
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(pub, msg, sig) {
			return nil
		}
	}
	return fmt.Errorf("the signature of %s isn't hakobu's: not installing it", tag)
}

// checksumOf finds file's SHA-256 in a checksums.txt.
func checksumOf(checksums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == file {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s isn't in the release's checksums", file)
}

// Download fetches tag's binary for this machine into dst (mode 0755)
// after checking the signature of the release's checksums and the
// binary's checksum; on any failure dst is removed.
func Download(tag, dst string) (err error) {
	if _, ok := parseTag(tag); !ok {
		return fmt.Errorf("%q isn't a release tag (like v1.2.3)", tag)
	}
	checksums, err := fetch(tag, checksumsFile, 1<<20)
	if err != nil {
		return err
	}
	sig, err := fetch(tag, signatureFile, 1<<10)
	if err != nil {
		return fmt.Errorf("%w (releases before signing can only be installed with install.sh)", err)
	}
	if err := VerifySignature(tag, checksums, sig); err != nil {
		return err
	}
	want, err := checksumOf(checksums, BinaryName())
	if err != nil {
		return err
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	resp, err := get(tag, BinaryName())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return fmt.Errorf("downloading %s %s: %w", BinaryName(), tag, err)
	}
	if hex.EncodeToString(sum.Sum(nil)) != want {
		return fmt.Errorf("%s %s doesn't match its signed checksum: not installing it", BinaryName(), tag)
	}
	return f.Sync()
}

func get(tag, file string) (*http.Response, error) {
	resp, err := client.Get(DownloadURL + "/" + Repo + "/releases/download/" + tag + "/" + file)
	if err != nil {
		return nil, fmt.Errorf("downloading %s of %s: %w", file, tag, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloading %s of %s: %s", file, tag, resp.Status)
	}
	return resp, nil
}

func fetch(tag, file string, limit int64) ([]byte, error) {
	resp, err := get(tag, file)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New(file + " is too large")
	}
	return b, nil
}
