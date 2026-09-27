package ops

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

// buildCacheKeep is how long unused build cache is kept: long enough that
// redeploys stay fast, short enough that the disk doesn't fill up.
const buildCacheKeep = 7 * 24 * time.Hour

func workDir(app string) string { return "data/work/" + app }

// removeAppImages deletes every image and the clone of a deleted app.
func removeAppImages(app string) {
	for _, tag := range []string{"latest", "previous", "next"} {
		deploy.RemoveImage(ctx(), "hakobu/"+app+":"+tag)
	}
	os.RemoveAll(workDir(app))
}

var (
	lastCleanupMu sync.Mutex
	lastCleanup   string
)

// LastCleanup describes the most recent Cleanup run, "" if none yet.
func LastCleanup() string {
	lastCleanupMu.Lock()
	defer lastCleanupMu.Unlock()
	return lastCleanup
}

// Cleanup frees disk: build cache unused for a week, and images and clones
// left over from apps that no longer exist.
func Cleanup(s *store.Store) error {
	result, err := cleanup(s)
	if err != nil {
		result = "failed: " + err.Error()
	}
	lastCleanupMu.Lock()
	lastCleanup = time.Now().Format("2006-01-02 15:04") + ": " + result
	lastCleanupMu.Unlock()
	return err
}

func cleanup(s *store.Store) (string, error) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		return "", err
	}
	exists := map[string]bool{}
	for _, a := range apps {
		exists[a.Name] = true
	}

	tags, err := deploy.ImageTags(ctx(), "hakobu/*")
	if err != nil {
		return "", err
	}
	removed := 0
	for _, tag := range tags {
		name := strings.TrimPrefix(tag[:strings.LastIndex(tag, ":")], "hakobu/")
		if !exists[name] {
			deploy.RemoveImage(ctx(), tag)
			removed++
		}
	}
	if clones, err := os.ReadDir("data/work"); err == nil {
		for _, c := range clones {
			if !IsDeploying(c.Name()) {
				os.RemoveAll(workDir(c.Name()))
			}
		}
	}

	freed, err := deploy.PruneBuildCache(ctx(), buildCacheKeep)
	return fmt.Sprintf("freed %s of build cache, removed %d image(s) of deleted apps", humanBytes(uint64(max(freed, 0))), removed), err
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// DiskUsage describes the disk Docker uses, e.g. "41.2 GB of 98.3 GB (42%)";
// low is true below 10% free.
func DiskUsage() (text string, low bool) {
	used, total, err := deploy.Disk(ctx())
	if err != nil || total == 0 {
		return "unknown", false
	}
	pct := used * 100 / total
	return fmt.Sprintf("%s of %s used (%d%%)", humanBytes(used), humanBytes(total), pct), pct >= 90
}
