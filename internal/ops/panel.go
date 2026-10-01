package ops

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// The panel's own database is backed up to the backup bucket too, sealed
// with the master key (secret.NewFileWriter), which never goes to the
// bucket: the owner downloads it from Settings (KeyFile). With the key file
// and a Cloudflare token, `hakobu restore` brings the panel back on a new
// server. A backup made before a rotation of the master key needs the key
// from before it.
const panelPrefix = "panel/"

// keepPanelBackups is how many panel backups the rotation keeps beyond the
// ones still under the bucket's lock.
const keepPanelBackups = 7

const panelTimeFormat = "20060102-150405"

var lastPanel struct {
	sync.Mutex
	at  time.Time // of the newest backup, zero if unknown
	err string    // of the last attempt, "" if it worked
}

// LastPanelBackup describes the newest panel backup for Settings.
func LastPanelBackup() string {
	lastPanel.Lock()
	defer lastPanel.Unlock()
	switch {
	case lastPanel.err != "":
		return "failed: " + lastPanel.err
	case lastPanel.at.IsZero():
		return ""
	}
	return lastPanel.at.UTC().Format("2006-01-02 15:04 UTC")
}

func setLastPanel(at time.Time, err error) {
	lastPanel.Lock()
	defer lastPanel.Unlock()
	lastPanel.err = ""
	if err != nil {
		lastPanel.err = err.Error()
		return
	}
	if at.After(lastPanel.at) {
		lastPanel.at = at
	}
}

// panelBackup is one backup in the bucket: its parts are key/000, key/001...
type panelBackup struct {
	key   string
	at    time.Time
	parts int
}

// listPanelBackups returns the panel backups in the bucket, newest first.
func listPanelBackups(c cloudflare.Client, account, bucket string) ([]panelBackup, error) {
	keys, err := c.ListObjects(account, bucket, panelPrefix)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*panelBackup{}
	for _, k := range keys {
		base := k[:strings.LastIndexByte(k, '/')]
		name := strings.TrimSuffix(strings.TrimPrefix(base, panelPrefix), ".db.enc")
		at, err := time.Parse(panelTimeFormat, name)
		if err != nil {
			continue // not one of ours
		}
		b := byKey[base]
		if b == nil {
			b = &panelBackup{key: base, at: at}
			byKey[base] = b
		}
		b.parts++
	}
	list := make([]panelBackup, 0, len(byKey))
	for _, b := range byKey {
		list = append(list, *b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
	return list, nil
}

// BackupPanel uploads a sealed copy of the panel's database and drops the
// backups the rotation no longer keeps.
func BackupPanel(s *store.Store) (err error) {
	defer func() { setLastPanel(time.Now(), err) }()
	if _, _, _, err := r2(s); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	copyPath := filepath.Join(tmpDir, fmt.Sprintf("panel-%d.db", time.Now().UnixNano()))
	defer os.Remove(copyPath)
	if err := s.SnapshotTo(copyPath); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}
	sealed, err := os.CreateTemp(tmpDir, "panel-*.db.enc")
	if err != nil {
		return err
	}
	defer os.Remove(sealed.Name())
	defer sealed.Close()
	if err := sealFile(copyPath, sealed); err != nil {
		return err
	}
	info, err := sealed.Stat()
	if err != nil {
		return err
	}
	key := panelPrefix + time.Now().UTC().Format(panelTimeFormat) + ".db.enc"
	if _, err := uploadParts(s, key, sealed, info.Size()); err != nil {
		return err
	}
	if err := rotatePanelBackups(s, time.Now()); err != nil {
		fmt.Println("panel backup rotation failed:", err)
	}
	return nil
}

func sealFile(path string, out io.Writer) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := secret.NewFileWriter(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		return err
	}
	return w.Close()
}

// rotatePanelBackups keeps the newest keepPanelBackups and every backup
// still under the bucket's lock.
func rotatePanelBackups(s *store.Store, now time.Time) error {
	c, account, bucket, err := r2(s)
	if err != nil {
		return err
	}
	list, err := listPanelBackups(c, account, bucket)
	if err != nil {
		return err
	}
	locked := now.AddDate(0, 0, -backupLockDays)
	for i, b := range list {
		if i < keepPanelBackups || b.at.After(locked) {
			continue
		}
		for p := range b.parts {
			if err := c.DeleteObject(account, bucket, partKey(b.key, p)); err != nil {
				return err // retried by the next rotation
			}
		}
	}
	return nil
}

// PanelBackupDue backs the panel up if its newest backup is older than
// config.BackupEvery; nothing happens before backups are set up.
func PanelBackupDue(s *store.Store) error {
	if BackupBucket(s) == "" {
		return nil
	}
	lastPanel.Lock()
	at := lastPanel.at
	lastPanel.Unlock()
	if at.IsZero() { // after a restart: ask the bucket
		c, account, bucket, err := r2(s)
		if err != nil {
			return err
		}
		list, err := listPanelBackups(c, account, bucket)
		if err != nil {
			return err
		}
		if len(list) > 0 {
			at = list[0].at
			setLastPanel(at, nil)
		}
	}
	if time.Since(at) < config.BackupEvery {
		return nil
	}
	return BackupPanel(s)
}

// KeyFile is what the owner downloads from Settings and gives `hakobu
// restore`: the master key, where the panel's backups are and the panel's
// address. Only the key is secret, and it's the one that matters.
type KeyFile struct {
	Key        string
	AccountID  string
	Bucket     string
	PublicHost string
	AppsDomain string
}

var keyFileFields = []struct {
	name string
	get  func(*KeyFile) *string
}{
	{"HAKOBU_MASTER_KEY", func(k *KeyFile) *string { return &k.Key }},
	{"HAKOBU_BACKUP_ACCOUNT", func(k *KeyFile) *string { return &k.AccountID }},
	{"HAKOBU_BACKUP_BUCKET", func(k *KeyFile) *string { return &k.Bucket }},
	{"HAKOBU_PUBLIC_HOST", func(k *KeyFile) *string { return &k.PublicHost }},
	{"HAKOBU_APPS_DOMAIN", func(k *KeyFile) *string { return &k.AppsDomain }},
}

// CurrentKeyFile describes this panel with the key it encrypts with now.
func CurrentKeyFile(s *store.Store) (KeyFile, error) {
	key, err := secret.ExportKey()
	if err != nil {
		return KeyFile{}, err
	}
	kf := KeyFile{Key: key, PublicHost: config.PublicHost(), AppsDomain: config.AppsDomain()}
	if cf, err := s.GetCloudflare(ctx()); err == nil {
		kf.AccountID, kf.Bucket = cf.AccountID, cf.BackupBucket
	}
	return kf, nil
}

func (k KeyFile) Marshal() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# hakobu master key of %s, %s\n", k.PublicHost, time.Now().UTC().Format("2006-01-02"))
	b.WriteString("# It decrypts every secret of the panel and its backups. Keep it somewhere\n")
	b.WriteString("# safe, away from the server; to bring the panel back on a new one:\n")
	b.WriteString("#   CLOUDFLARE_API_TOKEN=... hakobu restore --key this-file\n")
	for _, f := range keyFileFields {
		fmt.Fprintf(&b, "%s=%s\n", f.name, *f.get(&k))
	}
	return b.Bytes()
}

func ParseKeyFile(r io.Reader) (KeyFile, error) {
	var k KeyFile
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		for _, f := range keyFileFields {
			if f.name == name {
				*f.get(&k) = value
			}
		}
	}
	if err := sc.Err(); err != nil {
		return k, err
	}
	if k.Key == "" {
		return k, errors.New("not a hakobu key file: no HAKOBU_MASTER_KEY")
	}
	return k, nil
}

// KeyDownloaded reports whether the owner has downloaded the key secrets
// are encrypted with now; after a rotation it's a new one.
func KeyDownloaded() bool {
	b, err := os.ReadFile(keyDownloadedFile)
	return err == nil && strings.TrimSpace(string(b)) == secret.Fingerprint() && secret.Fingerprint() != ""
}

const keyDownloadedFile = "data/key_downloaded"

// MarkKeyDownloaded remembers which key the owner has downloaded.
func MarkKeyDownloaded() error {
	return os.WriteFile(keyDownloadedFile, []byte(secret.Fingerprint()+"\n"), 0o600)
}

// RestorePanel puts the newest panel backup (or the one made at, in
// panelTimeFormat) from the key file's bucket in place as dbPath, with the
// key in keyPath; neither may exist. It returns when that backup was made.
func RestorePanel(c cloudflare.Client, kf KeyFile, dbPath, keyPath, at string) (time.Time, error) {
	if kf.AccountID == "" || kf.Bucket == "" {
		return time.Time{}, errors.New("the key file names no backup bucket: backups weren't set up when it was downloaded")
	}
	if _, err := os.Stat(dbPath); err == nil {
		return time.Time{}, fmt.Errorf("%s exists: this server already has a panel; move it away to restore", dbPath)
	}
	list, err := listPanelBackups(c, kf.AccountID, kf.Bucket)
	if err != nil {
		return time.Time{}, err
	}
	var pick *panelBackup
	for i := range list {
		if at == "" || list[i].at.Format(panelTimeFormat) == at {
			pick = &list[i]
			break
		}
	}
	if pick == nil && at != "" {
		return time.Time{}, fmt.Errorf("no panel backup made at %s in %s", at, kf.Bucket)
	}
	if pick == nil {
		return time.Time{}, fmt.Errorf("no panel backup in %s", kf.Bucket)
	}

	if err := secret.ImportKey(keyPath, kf.Key); err != nil {
		return time.Time{}, err
	}
	restored := false
	defer func() {
		if !restored {
			os.Remove(keyPath)
		}
	}()
	if err := secret.LoadKey(keyPath); err != nil {
		return time.Time{}, err
	}
	body := &partsReader{parts: pick.parts, open: func(i int) (io.ReadCloser, error) {
		return c.GetObject(kf.AccountID, kf.Bucket, partKey(pick.key, i))
	}}
	defer body.Close()
	plain, err := secret.NewFileReader(body)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup %s: %w (is it the key file of this panel, from after its last key rotation?)", pick.key, err)
	}
	tmp := dbPath + ".restore"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return time.Time{}, err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(f, plain); err != nil {
		f.Close()
		return time.Time{}, fmt.Errorf("backup %s: %w", pick.key, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return time.Time{}, err
	}
	if err := f.Close(); err != nil {
		return time.Time{}, err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return time.Time{}, err
	}
	restored = true
	if kf.PublicHost != "" {
		if err := config.SetPublicHost(kf.PublicHost); err != nil {
			return pick.at, err
		}
	}
	if kf.AppsDomain != "" {
		if err := config.SetAppsDomain(kf.AppsDomain); err != nil {
			return pick.at, err
		}
	}
	return pick.at, nil
}
