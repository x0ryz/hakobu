package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
)

// The panel can't update hakobu itself: it runs as the unprivileged user
// hakobu, which can't write the binary, so a break-in into the panel can't
// plant a binary of its own. It asks by creating RequestFile; systemd
// (hakobu-update.path) then runs `hakobu update --requested` as root,
// which installs only the latest signed release. The worst a forged
// request does is that.

// RequestFile, under the install's directory, asks for an update.
const RequestFile = "data/update-request"

// Files of an install, under its directory. The binaries, the state and
// the status are root's; the database and its copy are hakobu's.
const (
	binaryFile   = "hakobu"
	prevFile     = "hakobu.prev"     // the binary before the last update
	downloadFile = "hakobu.download" // a release being checked
	stateFile    = "update-state.json"
	StatusFile   = "update-status.json" // read by the panel
	lockFile     = "update.lock"
	databaseFile = "data/hakobu.db"
	// PrevDatabase is the panel's database as it was before the last
	// update, for a rollback past a schema change.
	PrevDatabase = "data/hakobu.db.prev"
)

const unitFile = "/etc/systemd/system/hakobu.service"

// Install is hakobu as install.sh set it up on this server.
type Install struct {
	Dir      string    // /opt/hakobu
	Version  string    // of the binary installed now: the one running this
	Rootless bool      // runs as user hakobu, Docker as hakobu-docker
	Out      io.Writer // progress

	run     func(dir string, name string, args ...string) (string, error)
	healthy func() bool
	addr    string
}

// state is what an update leaves for a rollback.
type state struct {
	Previous       string    // version of the binary in prevFile
	PreviousSchema int       // the schema version it knows
	UpdatedAt      time.Time // when it was replaced
}

// Status of the last update, for the panel.
type Status struct {
	At      time.Time
	From    string
	To      string
	State   string // "running", "updated", "up to date", "failed", "rolled back"
	Message string
}

// Detect describes the install in dir, as root: its unit tells whether
// it runs as hakobu (rootless) or as root (installs from before that).
func Detect(dir, version, addr string, out io.Writer) (*Install, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("run it as root: sudo " + filepath.Join(dir, binaryFile) + " " + strings.Join(os.Args[1:], " "))
	}
	unit, err := os.ReadFile(unitFile)
	if err != nil {
		return nil, fmt.Errorf("hakobu isn't installed as a service here (%w); install it with install.sh", err)
	}
	return &Install{
		Dir: dir, Version: version, Out: out, addr: addr,
		Rootless: regexp.MustCompile(`(?m)^User=hakobu$`).Match(unit),
	}, nil
}

func (in *Install) path(name string) string { return filepath.Join(in.Dir, name) }

func (in *Install) logf(format string, args ...any) {
	fmt.Fprintf(in.Out, format+"\n", args...)
}

// lock keeps two updates or rollbacks (the panel's and one over SSH)
// from running at once.
func (in *Install) lock() (unlock func(), err error) {
	f, err := os.OpenFile(in.path(lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another update or rollback is running")
	}
	return func() { f.Close() }, nil
}

// Update installs release tag, or the latest release if tag is "" and
// it's newer than the installed one. If the new version doesn't come up,
// the previous one is put back.
func (in *Install) Update(tag string) error {
	unlock, err := in.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if tag == "" {
		latest, err := Latest()
		if err != nil {
			in.setStatus("failed", "", err.Error())
			return err
		}
		if !Newer(latest, in.Version) {
			in.logf("hakobu %s is the latest release", in.Version)
			in.setStatus("up to date", latest, "")
			return nil
		}
		tag = latest
	}
	in.setStatus("running", tag, "downloading and checking "+tag)
	in.logf("downloading hakobu %s and checking its signature", tag)
	if err := Download(tag, in.path(downloadFile)); err != nil {
		in.setStatus("failed", tag, err.Error())
		return err
	}

	in.logf("stopping hakobu %s (apps keep running)", in.Version)
	if err := in.systemctl("stop", "hakobu"); err != nil {
		os.Remove(in.path(downloadFile))
		in.setStatus("failed", tag, err.Error())
		return err
	}
	// The copy is made by the binary installed now, as the user it runs
	// as: root never writes into hakobu's data.
	if _, err := in.asHakobu(in.path(binaryFile), "snapshot-db", PrevDatabase); err != nil {
		os.Remove(in.path(downloadFile))
		in.startAgain()
		err = fmt.Errorf("copying the panel's database before the update failed, nothing changed: %w", err)
		in.setStatus("failed", tag, err.Error())
		return err
	}
	if err := in.saveState(state{Previous: in.Version, PreviousSchema: store.SchemaVersion(), UpdatedAt: time.Now().UTC()}); err != nil {
		in.startAgain()
		in.setStatus("failed", tag, err.Error())
		return err
	}
	if err := os.Rename(in.path(binaryFile), in.path(prevFile)); err != nil {
		in.startAgain()
		in.setStatus("failed", tag, err.Error())
		return err
	}
	if err := os.Rename(in.path(downloadFile), in.path(binaryFile)); err != nil {
		_ = os.Rename(in.path(prevFile), in.path(binaryFile))
		in.startAgain()
		in.setStatus("failed", tag, err.Error())
		return err
	}

	in.logf("starting hakobu %s", tag)
	if err := in.start(); err == nil && in.waitHealthy() {
		in.logf("hakobu %s is up; `hakobu rollback` goes back to %s", tag, in.Version)
		in.setStatus("updated", tag, "")
		return nil
	}
	in.logf("hakobu %s didn't come up, going back to %s", tag, in.Version)
	if err := in.rollback(); err != nil {
		err = fmt.Errorf("%s didn't come up, and going back to %s failed: %w", tag, in.Version, err)
		in.setStatus("failed", tag, err.Error())
		return err
	}
	err = fmt.Errorf("%s didn't come up (see journalctl -u hakobu), so %s was put back", tag, in.Version)
	in.setStatus("rolled back", tag, err.Error())
	return err
}

// Rollback puts back the binary from before the last update and, if that
// update changed the database's schema, the database as it was then.
func (in *Install) Rollback() error {
	unlock, err := in.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := in.loadState()
	if err != nil {
		return err
	}
	from := in.Version
	in.Version = st.Previous
	if err := in.rollback(); err != nil {
		in.Version = from
		return err
	}
	in.setStatusFrom(from, "rolled back", st.Previous, "")
	return nil
}

func (in *Install) rollback() error {
	st, err := in.loadState()
	if err != nil {
		return err
	}
	if err := in.systemctl("stop", "hakobu"); err != nil {
		return err
	}
	// The previous binary refuses a database migrated past what it knows:
	// then the copy from before the update goes back in, and the newer
	// database is kept beside it. Asked of the newer binary, as hakobu; if
	// it can't say, the copy goes back too.
	version, err := in.asHakobu(in.path(binaryFile), "schema-version", databaseFile)
	v, convErr := strconv.Atoi(strings.TrimSpace(version))
	if err != nil || convErr != nil || v > st.PreviousSchema {
		if _, err := os.Stat(in.path(PrevDatabase)); err != nil {
			in.startAgain()
			return fmt.Errorf("the update changed the panel's database and its copy from before the update is gone (a master key rotation drops it): %s can't run with it", st.Previous)
		}
		aside := databaseFile + ".rolled-back-" + time.Now().UTC().Format("20060102-150405")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(in.path(databaseFile+suffix), in.path(aside+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				in.startAgain()
				return err
			}
		}
		if err := os.Rename(in.path(PrevDatabase), in.path(databaseFile)); err != nil {
			in.startAgain()
			return err
		}
		in.logf("put back the panel's database from %s; the newer one is in %s", st.UpdatedAt.Format("2006-01-02 15:04 UTC"), aside)
	}
	if err := os.Rename(in.path(prevFile), in.path(binaryFile)); err != nil {
		in.startAgain()
		return err
	}
	os.Remove(in.path(stateFile))
	in.logf("starting hakobu %s", st.Previous)
	if err := in.start(); err != nil {
		return err
	}
	if !in.waitHealthy() {
		return fmt.Errorf("hakobu %s didn't come up either: see journalctl -u hakobu", st.Previous)
	}
	in.logf("hakobu %s is back", st.Previous)
	return nil
}

func (in *Install) saveState(st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(in.path(stateFile), b, 0o600)
}

func (in *Install) loadState() (state, error) {
	var st state
	b, err := os.ReadFile(in.path(stateFile))
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err == nil {
		_, err = os.Stat(in.path(prevFile))
	}
	if err != nil {
		return st, errors.New("there's no previous version to go back to: hakobu keeps one from its last update (`hakobu update`)")
	}
	return st, nil
}

func (in *Install) setStatus(stateName, to, msg string) {
	in.setStatusFrom(in.Version, stateName, to, msg)
}

func (in *Install) setStatusFrom(from, stateName, to, msg string) {
	b, _ := json.Marshal(Status{At: time.Now().UTC(), From: from, To: to, State: stateName, Message: msg})
	if err := writeFileAtomic(in.path(StatusFile), b, 0o644); err != nil {
		in.logf("couldn't save the update's status: %v", err)
	}
}

// ReadStatus reads the last update's status, as the panel does.
func ReadStatus(dir string) (Status, bool) {
	var st Status
	b, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

// writeFileAtomic writes path through a temporary file in the same
// directory, so a reader never sees half of it.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (in *Install) command(dir, name string, args ...string) (string, error) {
	if in.run != nil {
		return in.run(dir, name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (in *Install) systemctl(args ...string) error {
	_, err := in.command("/", "systemctl", args...)
	return err
}

// asHakobu runs bin in the install's directory as the user hakobu runs as.
func (in *Install) asHakobu(bin string, args ...string) (string, error) {
	if !in.Rootless {
		return in.command(in.Dir, bin, args...)
	}
	return in.command(in.Dir, "runuser", append([]string{"-u", "hakobu", "--", "env", "HOME=/home/hakobu", bin}, args...)...)
}

// start starts hakobu and, under rootless Docker, restarts the dialer,
// which runs the hakobu binary too.
func (in *Install) start() error {
	if in.Rootless {
		if err := in.restartDialer(); err != nil {
			return err
		}
	}
	return in.systemctl("start", "hakobu")
}

// restartDialer restarts the Docker user's dialer service; systemd before
// 248 can't reach another user's services with -M, so it's done as that
// user then.
func (in *Install) restartDialer() error {
	if in.systemctl("--user", "-M", "hakobu-docker@", "restart", "hakobu-dialer") == nil {
		return nil
	}
	u, err := user.Lookup("hakobu-docker")
	if err != nil {
		return err
	}
	run := "/run/user/" + u.Uid
	_, err = in.command("/", "runuser", "-u", "hakobu-docker", "--", "env", "XDG_RUNTIME_DIR="+run, "DBUS_SESSION_BUS_ADDRESS=unix:path="+run+"/bus",
		"systemctl", "--user", "restart", "hakobu-dialer")
	return err
}

func (in *Install) startAgain() {
	if err := in.start(); err != nil {
		in.logf("starting hakobu again failed: %v", err)
	}
}

// waitHealthy waits up to three minutes for the panel to answer.
func (in *Install) waitHealthy() bool {
	if in.healthy != nil {
		return in.healthy()
	}
	c := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		resp, err := c.Get("http://" + in.addr + "/login")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}
