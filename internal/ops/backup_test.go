package ops

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
