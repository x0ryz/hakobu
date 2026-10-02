package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, current string
		want         bool
	}{
		{"v0.7.0", "v0.6.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.6.0", "v0.6.0", false},
		{"v0.5.9", "v0.6.0", false},
		{"v0.7.0", "dev", true},
		{"latest", "v0.6.0", false},
		{"v0.7.0-rc1", "v0.6.0", false},
	} {
		if got := Newer(c.tag, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.tag, c.current, got)
		}
	}
}

// fakeRelease serves releases signed with a key of the test's own.
type fakeRelease struct {
	priv  ed25519.PrivateKey
	files map[string][]byte // "tag/file"
}

func newFakeRelease(t *testing.T) *fakeRelease {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := publicKeys
	publicKeys = []string{base64.StdEncoding.EncodeToString(pub)}
	f := &fakeRelease{priv: priv, files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/"+Repo+"/releases/latest" {
			fmt.Fprint(w, `{"tag_name": "v0.7.0"}`)
			return
		}
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/"+Repo+"/releases/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	oldAPI, oldDL := APIURL, DownloadURL
	APIURL, DownloadURL = srv.URL, srv.URL
	t.Cleanup(func() {
		srv.Close()
		publicKeys, APIURL, DownloadURL = old, oldAPI, oldDL
	})
	return f
}

// publish adds a signed release whose binary is body.
func (f *fakeRelease) publish(tag, body string) {
	sum := sha256.Sum256([]byte(body))
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + BinaryName() + "\n" + strings.Repeat("0", 64) + "  hakobu-linux-other\n")
	f.files[tag+"/"+BinaryName()] = []byte(body)
	f.files[tag+"/"+checksumsFile] = checksums
	f.files[tag+"/"+signatureFile] = ed25519.Sign(f.priv, SignedMessage(tag, checksums))
}

func TestDownloadChecksSignatureAndChecksum(t *testing.T) {
	f := newFakeRelease(t)
	f.publish("v0.6.0", "old binary")
	f.publish("v0.7.0", "new binary")
	dst := filepath.Join(t.TempDir(), "hakobu.download")

	if err := Download("v0.7.0", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new binary" {
		t.Errorf("downloaded %q", b)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode())
	}

	refused := func(what string) {
		t.Helper()
		if err := Download("v0.7.0", dst); err == nil {
			t.Errorf("%s: installed", what)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the download was left behind", what)
		}
	}
	good := map[string][]byte{}
	for k, v := range f.files {
		good[k] = v
	}
	reset := func() {
		for k, v := range good {
			f.files[k] = v
		}
	}

	f.files["v0.7.0/"+BinaryName()] = []byte("new binarY")
	refused("a binary that isn't the signed one")
	reset()

	f.files["v0.7.0/"+checksumsFile] = append([]byte(nil), f.files["v0.6.0/"+checksumsFile]...)
	f.files["v0.7.0/"+BinaryName()] = f.files["v0.6.0/"+BinaryName()]
	refused("checksums from another release without its signature")
	// The whole of an older release, signature included, under a newer tag.
	f.files["v0.7.0/"+signatureFile] = f.files["v0.6.0/"+signatureFile]
	refused("an older release passed off as v0.7.0")
	reset()

	delete(f.files, "v0.7.0/"+signatureFile)
	refused("an unsigned release")
	reset()

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	f.files["v0.7.0/"+signatureFile] = ed25519.Sign(other, SignedMessage("v0.7.0", f.files["v0.7.0/"+checksumsFile]))
	refused("a release signed with another key")
	reset()

	if err := Download("../../etc", dst); err == nil {
		t.Error("an odd tag was downloaded")
	}
}

// fakeInstall is an install in a temporary directory with systemd and the
// binaries' commands faked.
type fakeInstall struct {
	*Install
	commands []string
	schema   string // what the installed binary says of the database
	healthy  []bool // answers of successive health checks
}

func newFakeInstall(t *testing.T, version string) *fakeInstall {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, binaryFile), []byte(version))
	mustWrite(t, filepath.Join(dir, databaseFile), []byte("db of "+version))
	f := &fakeInstall{schema: "0"}
	f.Install = &Install{Dir: dir, Version: version, Rootless: true, Out: io.Discard}
	f.run = func(_ string, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		f.commands = append(f.commands, cmd)
		switch {
		case strings.Contains(cmd, "snapshot-db"):
			b, _ := os.ReadFile(filepath.Join(dir, databaseFile))
			return "", os.WriteFile(filepath.Join(dir, PrevDatabase), b, 0o600)
		case strings.Contains(cmd, "schema-version"):
			return f.schema + "\n", nil
		}
		return "", nil
	}
	f.Install.healthy = func() bool {
		ok := f.healthy[0]
		f.healthy = f.healthy[1:]
		return ok
	}
	return f
}

func (f *fakeInstall) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.Dir, name))
	return string(b)
}

func TestUpdateAndRollback(t *testing.T) {
	r := newFakeRelease(t)
	r.publish("v0.7.0", "v0.7.0")
	f := newFakeInstall(t, "v0.6.0")
	f.healthy = []bool{true}

	if err := f.Update(""); err != nil {
		t.Fatal(err)
	}
	if f.read(binaryFile) != "v0.7.0" || f.read(prevFile) != "v0.6.0" {
		t.Errorf("binaries %q, previous %q", f.read(binaryFile), f.read(prevFile))
	}
	if f.read(PrevDatabase) != "db of v0.6.0" {
		t.Error("no copy of the database from before the update")
	}
	cmds := strings.Join(f.commands, "\n")
	for _, want := range []string{
		"systemctl stop hakobu",
		"runuser -u hakobu -- env HOME=/home/hakobu " + filepath.Join(f.Dir, binaryFile) + " snapshot-db " + PrevDatabase,
		"systemctl --user -M hakobu-docker@ restart hakobu-dialer",
		"systemctl start hakobu",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in:\n%s", want, cmds)
		}
	}
	if st, _ := ReadStatus(f.Dir); st.State != "updated" || st.From != "v0.6.0" || st.To != "v0.7.0" {
		t.Errorf("status %+v", st)
	}
	if info, _ := os.Stat(filepath.Join(f.Dir, StatusFile)); info.Mode().Perm() != 0o644 {
		t.Errorf("the panel can't read the status: %v", info.Mode())
	}

	// The panel was used after the update; no schema change: its database
	// stays.
	mustWrite(t, filepath.Join(f.Dir, databaseFile), []byte("db written by v0.7.0"))
	f.Version = "v0.7.0"
	f.healthy = []bool{true}
	if err := f.Rollback(); err != nil {
		t.Fatal(err)
	}
	if f.read(binaryFile) != "v0.6.0" || f.read(databaseFile) != "db written by v0.7.0" {
		t.Errorf("after rollback: binary %q, db %q", f.read(binaryFile), f.read(databaseFile))
	}
	if st, _ := ReadStatus(f.Dir); st.State != "rolled back" || st.To != "v0.6.0" {
		t.Errorf("status %+v", st)
	}
	f.Version = "v0.6.0"
	if err := f.Rollback(); err == nil {
		t.Error("rolled back twice")
	}
	f.healthy = []bool{true}
	if err := f.Update(""); err != nil || f.read(binaryFile) != "v0.7.0" {
		t.Fatalf("updating again: %v", err)
	}
}

func TestRollbackPastASchemaChange(t *testing.T) {
	r := newFakeRelease(t)
	r.publish("v0.7.0", "v0.7.0")
	f := newFakeInstall(t, "v0.6.0")
	f.healthy = []bool{true}
	if err := f.Update(""); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.Dir, databaseFile), []byte("migrated by v0.7.0"))
	mustWrite(t, filepath.Join(f.Dir, databaseFile+"-wal"), []byte("wal"))
	f.schema = "999" // past what v0.6.0 knows
	f.Version = "v0.7.0"
	f.healthy = []bool{true}
	if err := f.Rollback(); err != nil {
		t.Fatal(err)
	}
	if f.read(databaseFile) != "db of v0.6.0" {
		t.Errorf("database %q, want the copy from before the update", f.read(databaseFile))
	}
	if _, err := os.Stat(filepath.Join(f.Dir, databaseFile+"-wal")); err == nil {
		t.Error("the newer database's WAL was left next to the copy")
	}
	aside, _ := filepath.Glob(filepath.Join(f.Dir, "data", "hakobu.db.rolled-back-*"))
	if len(aside) != 2 {
		t.Errorf("the newer database and its WAL should be kept aside: %v", aside)
	}
}

func TestUpdateThatDoesntComeUpIsUndone(t *testing.T) {
	r := newFakeRelease(t)
	r.publish("v0.7.0", "v0.7.0")
	f := newFakeInstall(t, "v0.6.0")
	f.healthy = []bool{false, true}
	f.schema = "999"
	if err := f.Update(""); err == nil {
		t.Fatal("a version that didn't come up was reported as updated")
	}
	if f.read(binaryFile) != "v0.6.0" || f.read(databaseFile) != "db of v0.6.0" {
		t.Errorf("binary %q, database %q", f.read(binaryFile), f.read(databaseFile))
	}
	if st, _ := ReadStatus(f.Dir); st.State != "rolled back" {
		t.Errorf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, prevFile)); err == nil {
		t.Error("the broken version is still offered for rollback")
	}
}

func TestUpdateRefusesABadRelease(t *testing.T) {
	r := newFakeRelease(t)
	r.publish("v0.7.0", "v0.7.0")
	r.files["v0.7.0/"+BinaryName()] = []byte("tampered")
	f := newFakeInstall(t, "v0.6.0")
	if err := f.Update(""); err == nil {
		t.Fatal("a tampered release was installed")
	}
	if f.read(binaryFile) != "v0.6.0" || len(f.commands) != 0 {
		t.Errorf("binary %q, commands run: %v", f.read(binaryFile), f.commands)
	}
	if st, _ := ReadStatus(f.Dir); st.State != "failed" {
		t.Errorf("status %+v", st)
	}
}

// TestReleaseKeysAgree keeps the signing key's public half the same in
// the repository, install.sh and here.
func TestReleaseKeysAgree(t *testing.T) {
	pem, err := os.ReadFile("../../scripts/release-key.pub")
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Split(strings.TrimSpace(string(pem)), "\n")[1:2], ""))
	if err != nil || len(der) != 44 {
		t.Fatalf("release-key.pub: %v", err)
	}
	if raw := base64.StdEncoding.EncodeToString(der[12:]); !slices.Contains(publicKeys, raw) {
		t.Errorf("scripts/release-key.pub (%s) isn't in publicKeys", raw)
	}
	install, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(install), strings.TrimSpace(string(pem))) {
		t.Error("install.sh doesn't hold scripts/release-key.pub")
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
