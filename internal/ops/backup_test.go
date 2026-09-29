package ops

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestBackupsToDrop(t *testing.T) {
	now := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	// One backup a day for 40 days, newest first; the one 20 days ago is the
	// newest that restored in its check.
	var backups []store.Backup
	for day := range 40 {
		b := store.Backup{ID: int64(40 - day), CreatedAt: now.AddDate(0, 0, -day).Format("2006-01-02T15:04:05Z")}
		if day == 20 {
			b.VerifiedAt = "x"
		}
		if day < 20 {
			b.VerifiedAt, b.VerifyError = "x", "broken"
		}
		backups = append(backups, b)
	}
	var kept []int
	dropped := backupsToDrop(backups, 7, now)
	for day, b := range backups {
		if !slices.ContainsFunc(dropped, func(d store.Backup) bool { return d.ID == b.ID }) {
			kept = append(kept, day)
		}
	}
	// Days 0-6, the newest of each older ISO week within 28 days (the
	// Sundays 9, 16 and 23 days back; 2026-09-29 is a Tuesday) and day 20,
	// the newest that restored.
	want := []int{0, 1, 2, 3, 4, 5, 6, 9, 16, 20, 23}
	if !slices.Equal(kept, want) {
		t.Errorf("kept days %v, want %v", kept, want)
	}
}

func TestDBJobs(t *testing.T) {
	release := make(chan struct{})
	if err := startDBJob("main", "backing up", func() error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := startDBJob("main", "restoring", func() error { return nil }); err == nil {
		t.Error("a second job should be refused while one runs")
	}
	if j := DatabaseJob("main"); j.Running != "backing up" {
		t.Errorf("running = %q", j.Running)
	}
	close(release)
	for deadline := time.Now().Add(time.Second); DatabaseJob("main").Running != ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
	}
	if j := DatabaseJob("main"); j.Failed || j.Last == "" {
		t.Errorf("last = %+v", j)
	}
}

func TestBuildDir(t *testing.T) {
	clone := t.TempDir()
	os.MkdirAll(filepath.Join(clone, "web"), 0o755)
	os.Symlink("/etc", filepath.Join(clone, "escape"))
	os.Symlink("web", filepath.Join(clone, "alias"))
	for path, ok := range map[string]bool{"": true, ".": true, "/web/": true, "alias": true, "escape": false, "../..": false, "missing": false} {
		if _, err := buildDir(clone, path); (err == nil) != ok {
			t.Errorf("buildDir(%q) error = %v", path, err)
		}
	}
}

// fakeR2 is the part of Cloudflare's API backups use, keeping objects in
// memory.
func fakeR2(t *testing.T) (objects map[string][]byte, lock *string) {
	objects, lock = map[string][]byte{}, new(string)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const prefix = "/accounts/acc/r2/buckets"
		ok := `{"success":true,"errors":[],"messages":[],"result":{}}`
		path := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
		_, key, isObject := strings.Cut(path, "/objects/")
		key, _ = url.PathUnescape(key)
		switch {
		case r.Method == "POST" && path == "":
			fmt.Fprint(w, ok)
		case r.Method == "PUT" && strings.HasSuffix(path, "/lock"):
			b, _ := io.ReadAll(r.Body)
			*lock = string(b)
			fmt.Fprint(w, ok)
		case isObject && r.Method == "PUT":
			if r.ContentLength > cloudflare.MaxObjectSize {
				t.Errorf("upload of %d bytes", r.ContentLength)
			}
			objects[key], _ = io.ReadAll(r.Body)
			fmt.Fprint(w, ok)
		case isObject && r.Method == "GET":
			if b, found := objects[key]; found {
				w.Write(b)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10007,"message":"not found"}]}`)
		case isObject && r.Method == "DELETE":
			delete(objects, key)
			fmt.Fprint(w, ok)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })
	return objects, lock
}

func TestR2Backups(t *testing.T) {
	objects, lock := fakeR2(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SaveCloudflareToken(ctx(), store.SaveCloudflareTokenParams{AccessToken: "tok", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"})

	if _, err := uploadParts(s, "k", strings.NewReader("x"), 1); err == nil {
		t.Error("uploaded before backups were set up")
	}
	if err := SetupBackups(s); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(BackupBucket(s), "hakobu-backups-") || !strings.Contains(*lock, `"maxAgeSeconds":604800`) {
		t.Errorf("bucket %q, lock %s", BackupBucket(s), *lock)
	}

	// A dump over the part size goes up in parts and reads back whole.
	old := backupPartSize
	backupPartSize = 10
	t.Cleanup(func() { backupPartSize = old })
	dump := strings.Repeat("0123456789", 3) + "tail"
	parts, err := uploadParts(s, "main/1.sql.gz", strings.NewReader(dump), int64(len(dump)))
	if err != nil || parts != 4 || string(objects["main/1.sql.gz/003"]) != "tail" {
		t.Fatalf("parts = %d, %v, objects %v", parts, err, objects)
	}
	b := store.Backup{ObjectKey: "main/1.sql.gz", Parts: int64(parts)}
	got, err := io.ReadAll(&partsReader{s: s, b: b})
	if err != nil || string(got) != dump {
		t.Errorf("read back %q, %v", got, err)
	}
	// An empty dump still makes one (empty) part, so it can be read.
	if parts, _ := uploadParts(s, "empty", strings.NewReader(""), 0); parts != 1 {
		t.Errorf("empty dump: %d parts", parts)
	}

	// Rotation deletes every part of a dropped backup: this one, once seven
	// newer ones exist.
	id, err := s.CreateBackup(ctx(), store.CreateBackupParams{Database: "main", ObjectKey: b.ObjectKey, Parts: b.Parts})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		s.CreateBackup(ctx(), store.CreateBackupParams{Database: "main", ObjectKey: fmt.Sprint("newer", i), Parts: 0})
	}
	if err := RotateBackups(s, "main", time.Now().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for k := range objects {
		if strings.HasPrefix(k, "main/") {
			t.Errorf("%s left after rotation", k)
		}
	}
	if _, err := s.GetBackup(ctx(), id); err == nil {
		t.Error("backup still listed")
	}
}
