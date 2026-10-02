package ops

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/update"
)

// hakobu updates itself through systemd (internal/update): the panel only
// asks, by creating update.RequestFile.

// updaterUnit is installed by install.sh; without it a request would
// never be picked up.
const updaterUnit = "/etc/systemd/system/hakobu-update.path"

// UpdateInfo is what Settings shows about updates.
type UpdateInfo struct {
	Current   string
	Latest    string // "" if GitHub couldn't be asked
	Available bool   // Latest is newer than Current
	Updater   bool   // the Update button works on this server
	Requested bool   // asked for, not picked up yet
	Last      update.Status
	HasLast   bool
}

var latest struct {
	sync.Mutex
	tag      string
	asked    time.Time
	checking bool
}

// latestRelease returns the latest release known, asking GitHub in the
// background at most every few hours (every few minutes until it answers);
// "" until it has.
func latestRelease() string {
	latest.Lock()
	defer latest.Unlock()
	every := 6 * time.Hour
	if latest.tag == "" {
		every = 5 * time.Minute
	}
	if !latest.checking && time.Since(latest.asked) > every {
		latest.asked, latest.checking = time.Now(), true
		go func() {
			tag, err := update.Latest()
			latest.Lock()
			defer latest.Unlock()
			latest.checking = false
			if err == nil {
				latest.tag = tag
			}
		}()
	}
	return latest.tag
}

// Updates describes the running version and the latest release.
func Updates(current string) UpdateInfo {
	info := UpdateInfo{Current: update.Tag(current), Latest: latestRelease()}
	info.Available = info.Latest != "" && update.Newer(info.Latest, current)
	_, err := os.Stat(updaterUnit)
	info.Updater = err == nil
	_, err = os.Stat(update.RequestFile)
	info.Requested = err == nil
	info.Last, info.HasLast = update.ReadStatus(".")
	return info
}

// RequestUpdate asks systemd to install the latest signed release.
func RequestUpdate() error {
	if _, err := os.Stat(updaterUnit); err != nil {
		return errors.New("this server has no updater yet: update once with install.sh, which sets it up")
	}
	return os.WriteFile(update.RequestFile, nil, 0o600)
}
