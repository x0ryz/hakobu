package ops

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/update"
)

// hakobu updates itself through systemd (internal/update): the panel only
// asks, by creating update.RequestFile.

// UpdateInfo is what Settings shows about updates.
type UpdateInfo struct {
	Current   string
	Latest    string // "" if GitHub couldn't be asked
	Available bool   // Latest is newer than Current
	Updater   bool   // the Update button works on this server
	Requested bool   // an update or rollback asked for, not picked up yet
	Last      update.Status
	HasLast   bool

	// Roll back: the version before the last update, when there is one
	// and this server picks up the request.
	Previous    string
	UpdatedAt   string
	CheckedAt   string // when GitHub last told the latest release
	CanRollBack bool
	// RollbackLosesData: the update changed the database's schema, so a
	// rollback puts back its copy from before the update.
	RollbackLosesData bool
	// RollbackBlocked: why it can't go back, "" if it can.
	RollbackBlocked string
}

var latest struct {
	sync.Mutex
	tag      string
	asked    time.Time
	checked  time.Time // when GitHub last answered
	checking bool
}

// latestRelease returns the latest release known, asking GitHub in the
// background at most every hour (every few minutes until it answers); ""
// until it has.
func latestRelease() string {
	latest.Lock()
	defer latest.Unlock()
	every := time.Hour
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
				latest.tag, latest.checked = tag, time.Now()
			}
		}()
	}
	return latest.tag
}

// CheckForUpdates asks GitHub for the latest release right away.
func CheckForUpdates() error {
	tag, err := update.Latest()
	if err != nil {
		return err
	}
	latest.Lock()
	defer latest.Unlock()
	latest.tag, latest.asked, latest.checked = tag, time.Now(), time.Now()
	return nil
}

func latestChecked() string {
	latest.Lock()
	defer latest.Unlock()
	if latest.checked.IsZero() {
		return ""
	}
	return latest.checked.UTC().Format("2006-01-02 15:04 UTC")
}

// Updates describes the running version and the latest release.
func Updates(current string) UpdateInfo {
	info := UpdateInfo{Current: update.Tag(current), Latest: latestRelease(), CheckedAt: latestChecked()}
	info.Available = info.Latest != "" && update.Newer(info.Latest, current)
	info.Updater = update.UnitsInstalled("hakobu-update.path")
	for _, f := range []string{update.RequestFile, update.RollbackRequestFile} {
		if _, err := os.Stat(f); err == nil {
			info.Requested = true
		}
	}
	info.Last, info.HasLast = update.ReadStatus(".")
	if st, ok := update.ReadState("."); ok && update.UnitsInstalled("hakobu-rollback.path") {
		info.Previous, info.UpdatedAt, info.CanRollBack = update.Tag(st.Previous), st.UpdatedAt.Format("2006-01-02 15:04 UTC"), true
		// This hakobu migrated the database to its schema when it started.
		if store.SchemaVersion() > st.PreviousSchema {
			info.RollbackLosesData = true
			if _, err := os.Stat(update.PrevDatabase); err != nil {
				info.RollbackBlocked = "the update changed the panel's database and its copy from before it is gone (a master key rotation drops it)"
			}
		}
	}
	return info
}

// RequestUpdate asks systemd to install the latest signed release.
func RequestUpdate() error {
	if !update.UnitsInstalled("hakobu-update.path") {
		return errors.New("this server has no updater yet: update once with install.sh, which sets it up")
	}
	return os.WriteFile(update.RequestFile, nil, 0o600)
}

// RequestRollback asks systemd to put back the version from before the
// last update.
func RequestRollback() error {
	info := Updates("")
	switch {
	case !info.CanRollBack:
		return errors.New("there's no previous version to go back to")
	case info.RollbackBlocked != "":
		return errors.New(info.RollbackBlocked)
	}
	return os.WriteFile(update.RollbackRequestFile, nil, 0o600)
}
