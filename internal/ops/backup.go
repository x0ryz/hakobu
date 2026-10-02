package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Backups go to one R2 bucket in the connected Cloudflare account, written
// through the REST API with hakobu's Cloudflare API token: no S3 keys exist for it,
// so nothing an app holds can reach the backups. The bucket's lock keeps
// every file for backupLockDays, even from hakobu itself. Every backup is
// sealed (secret.NewFileWriterKey), so the bucket alone reveals nothing.
const backupLockDays = 7

// backupPartSize is under cloudflare.MaxObjectSize; tests make it smaller.
var backupPartSize int64 = 256 << 20

// BackupBucket is the R2 bucket backups go to, "" until they're set up.
func BackupBucket(s *store.Store) string {
	cf, err := s.GetCloudflare(ctx())
	if err != nil {
		return ""
	}
	return cf.BackupBucket
}

// SetupBackups creates the locked backup bucket; from then on every
// database is backed up daily.
func SetupBackups(s *store.Store) error {
	if BackupBucket(s) != "" {
		return nil
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	suffix, err := RandomHex(4)
	if err != nil {
		return err
	}
	bucket := "hakobu-backups-" + suffix
	if err := c.CreateBucket(cf.AccountID, bucket); err != nil {
		return fmt.Errorf("creating the R2 bucket failed (is R2 enabled in the Cloudflare dashboard?): %w", err)
	}
	if err := c.LockBucket(cf.AccountID, bucket, backupLockDays); err != nil {
		return err
	}
	return s.SetBackupBucket(ctx(), bucket)
}

// r2 returns an API client with a fresh token, the account and the backup
// bucket. It's called per request: a long upload can outlive a token.
func r2(s *store.Store) (cloudflare.Client, string, string, error) {
	c, cf, err := cfClient(s)
	if err != nil {
		return c, "", "", err
	}
	if cf.BackupBucket == "" {
		return c, "", "", errors.New("backups aren't set up yet (Settings → Backups)")
	}
	return c, cf.AccountID, cf.BackupBucket, nil
}

func partKey(b string, i int) string { return fmt.Sprintf("%s/%03d", b, i) }

// partsReader reads a backup's parts one after another as one stream.
type partsReader struct {
	parts   int
	open    func(part int) (io.ReadCloser, error)
	next    int
	current io.ReadCloser
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.current == nil {
			if r.next == r.parts {
				return 0, io.EOF
			}
			var err error
			if r.current, err = r.open(r.next); err != nil {
				return 0, err
			}
			r.next++
		}
		n, err := r.current.Read(p)
		if err == io.EOF {
			r.current.Close()
			r.current, err = nil, nil
			if n == 0 {
				continue
			}
		}
		return n, err
	}
}

func (r *partsReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}

// Backup jobs: one backup, check or restore per database at a time; the
// panel shows the running one and how the last one went.

type DBJob struct {
	Running string // "backing up", ...; "" when idle
	Last    string // how the last job ended, "" if none since the agent started
	Failed  bool
}

var (
	dbJobsMu sync.Mutex
	dbJobs   = map[string]*DBJob{}
)

func DatabaseJob(name string) DBJob {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	if j := dbJobs[name]; j != nil {
		return *j
	}
	return DBJob{}
}

func reserveDB(name, what string) error {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	j := dbJobs[name]
	if j == nil {
		j = &DBJob{}
		dbJobs[name] = j
	}
	if j.Running != "" {
		return fmt.Errorf("%s is busy %s, try again when it finishes", name, j.Running)
	}
	j.Running = what
	return nil
}

func finishDB(name, what string, err error) {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	j := dbJobs[name]
	j.Running, j.Failed = "", err != nil
	j.Last = time.Now().Format("2006-01-02 15:04") + ": " + what + " "
	if err != nil {
		j.Last += "failed: " + err.Error()
	} else {
		j.Last += "done"
	}
}

func runDBJob(name, what string, fn func() error) error {
	if err := reserveDB(name, what); err != nil {
		return err
	}
	err := fn()
	finishDB(name, what, err)
	return err
}

// startDBJob runs the job in the background; it fails at once if the
// database is busy.
func startDBJob(name, what string, fn func() error) error {
	if err := reserveDB(name, what); err != nil {
		return err
	}
	go func() { finishDB(name, what, fn()) }()
	return nil
}

// StartBackup backs the database up in the background.
func StartBackup(s *store.Store, dbName string) error {
	if _, err := s.GetDatabase(ctx(), dbName); err != nil {
		return err
	}
	return startDBJob(dbName, "backing up", func() error { return backupAndCheck(s, dbName) })
}

// StartVerify restores a backup into a scratch database in the background.
func StartVerify(s *store.Store, dbName string, backupID int64) error {
	b, err := dbBackup(s, dbName, backupID)
	if err != nil {
		return err
	}
	return startDBJob(dbName, "checking a backup", func() error { return VerifyBackup(s, b.ID) })
}

// StartRestore replays a backup into the database in the background.
func StartRestore(s *store.Store, dbName string, backupID int64) error {
	b, err := dbBackup(s, dbName, backupID)
	if err != nil {
		return err
	}
	return startDBJob(dbName, "restoring the backup of "+b.CreatedAt, func() error { return restoreBackup(s, b) })
}

// dbBackup looks up a backup and makes sure it belongs to dbName.
func dbBackup(s *store.Store, dbName string, id int64) (store.Backup, error) {
	b, err := s.GetBackup(ctx(), id)
	if err != nil || b.Database != dbName {
		return b, fmt.Errorf("no such backup of %s", dbName)
	}
	return b, nil
}

// backupAndCheck uploads a backup, restores it into a scratch database to
// prove it's usable, then drops old backups.
func backupAndCheck(s *store.Store, dbName string) error {
	id, err := BackupDatabase(s, dbName)
	if err != nil {
		return err
	}
	verifyErr := VerifyBackup(s, id)
	if err := RotateBackups(s, dbName, time.Now()); err != nil {
		fmt.Println("backup rotation of", dbName, "failed:", err)
	}
	if verifyErr != nil {
		return fmt.Errorf("the backup was uploaded, but restoring it failed: %w", verifyErr)
	}
	return nil
}

// BackupDatabase streams a pg_dump, sealed, through a temporary file (an
// upload needs its size up front) to R2.
func BackupDatabase(s *store.Store, dbName string) (id int64, err error) {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%s/%s.dump.enc", d.Name, time.Now().UTC().Format("20060102-150405"))
	obj, err := uploadSealed(s, key, func(w io.Writer) error {
		return backup.DumpDatabase(ctx(), PostgresContainer, d.User, d.Name, w)
	})
	if err != nil {
		return 0, err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{
		Database: d.Name, ObjectKey: key, Parts: int64(obj.parts), SizeBytes: obj.size, SHA256: obj.sha256, FileKey: secret.String(obj.fileKey),
	})
}

// sealedObject is a backup as uploaded: sealed with a key of its own,
// which is kept in the database encrypted with the master key, and the
// SHA-256 of the sealed file.
type sealedObject struct {
	parts   int
	size    int64
	sha256  string
	fileKey string
}

// uploadSealed seals what write produces into a temporary file on the data
// disk and uploads it as key's parts. A backup in the bucket is no use to
// whoever gets at the bucket without the master key.
func uploadSealed(s *store.Store, key string, write func(io.Writer) error) (sealedObject, error) {
	if _, _, _, err := r2(s); err != nil {
		return sealedObject{}, err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return sealedObject{}, err
	}
	f, err := os.CreateTemp(tmpDir, "backup-*.enc")
	if err != nil {
		return sealedObject{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha256.New()
	w, fileKey, err := secret.NewFileWriterKey(io.MultiWriter(f, sum))
	if err != nil {
		return sealedObject{}, err
	}
	if err := write(w); err != nil {
		return sealedObject{}, err
	}
	if err := w.Close(); err != nil {
		return sealedObject{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return sealedObject{}, err
	}
	parts, err := uploadParts(s, key, f, info.Size())
	if err != nil {
		return sealedObject{}, err
	}
	return sealedObject{parts: parts, size: info.Size(), sha256: hex.EncodeToString(sum.Sum(nil)), fileKey: fileKey}, nil
}

// tempReader is a temporary file read through r (the backup opened),
// removed on Close.
type tempReader struct {
	io.Reader
	f *os.File
}

func (t tempReader) Close() error {
	err := t.f.Close()
	os.Remove(t.f.Name())
	return err
}

// fetchSealed downloads a backup into a temporary file and checks it
// against the SHA-256 taken when it was made, so a backup changed in the
// bucket is never restored; it's read through its own key, or as it is
// for a dump from before backups were sealed (fileKey ""). The caller
// closes it, which removes the file.
func fetchSealed(s *store.Store, objectKey string, parts int64, sha, fileKey string) (io.ReadCloser, error) {
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, "restore-*")
	if err != nil {
		return nil, err
	}
	body := &partsReader{parts: int(parts), open: func(i int) (io.ReadCloser, error) {
		c, acc, bucket, err := r2(s) // per part: a long download can outlive a token
		if err != nil {
			return nil, err
		}
		return c.GetObject(acc, bucket, partKey(objectKey, i))
	}}
	defer body.Close()
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), body)
	if err == nil && hex.EncodeToString(sum.Sum(nil)) != sha {
		err = fmt.Errorf("the backup in the bucket doesn't match the one hakobu made (SHA-256 differs), not restoring it")
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	var r io.Reader = f
	if err == nil && fileKey != "" {
		r, err = secret.NewFileReaderKey(f, fileKey)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return tempReader{r, f}, nil
}

// fetchBackup is fetchSealed for a database backup.
func fetchBackup(s *store.Store, b store.Backup) (io.ReadCloser, error) {
	return fetchSealed(s, b.ObjectKey, b.Parts, b.SHA256, string(b.FileKey))
}

// uploadParts uploads size bytes of r as key/000, key/001, ...
func uploadParts(s *store.Store, key string, r io.ReaderAt, size int64) (parts int, err error) {
	for offset := int64(0); offset < size || parts == 0; offset += backupPartSize {
		c, acc, bucket, err := r2(s)
		if err != nil {
			return 0, err
		}
		n := min(backupPartSize, size-offset)
		if err := c.PutObject(acc, bucket, partKey(key, parts), io.NewSectionReader(r, offset, n), n); err != nil {
			return 0, err
		}
		parts++
	}
	return parts, nil
}

// tmpDir holds dumps on their way to storage; it's on the data disk rather
// than /tmp, which may be a small RAM disk.
const tmpDir = "data/tmp"

// VerifyBackup downloads a backup and restores it into a scratch database
// next to the real one, then drops it. The result is saved on the backup.
func VerifyBackup(s *store.Store, id int64) error {
	b, err := s.GetBackup(ctx(), id)
	if err != nil {
		return err
	}
	tables, verifyErr := verify(s, b)
	msg := ""
	if verifyErr != nil {
		msg = verifyErr.Error()
	}
	if err := s.SetBackupVerified(ctx(), store.SetBackupVerifiedParams{
		ID: id, VerifiedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), VerifyError: msg, Tables: int64(tables),
	}); err != nil {
		return err
	}
	return verifyErr
}

func verify(s *store.Store, b store.Backup) (tables int, err error) {
	d, err := s.GetDatabase(ctx(), b.Database)
	if err != nil {
		return 0, err
	}
	body, err := fetchBackup(s, b)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.verify." + d.Name
	drop := fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, scratch)
	for _, sql := range []string{
		drop, // left over from an interrupted check
		fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, scratch, d.User),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM PUBLIC`, scratch),
	} {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, sql); err != nil {
			return 0, err
		}
	}
	defer func() {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, drop); err != nil {
			fmt.Println("failed to drop", scratch+":", err)
		}
	}()
	if err := backup.RestoreDatabase(ctx(), PostgresContainer, d.User, scratch, body); err != nil {
		return 0, err
	}
	return backup.CountTables(ctx(), PostgresContainer, d.User, scratch)
}

// restoreBackup replays a backup into its database. Objects and rows that
// already exist cause errors rather than being overwritten.
func restoreBackup(s *store.Store, b store.Backup) error {
	d, err := s.GetDatabase(ctx(), b.Database)
	if err != nil {
		return err
	}
	body, err := fetchBackup(s, b)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := ensureInPostgres(s, d); err != nil {
		return err
	}
	return backup.RestoreDatabase(ctx(), PostgresContainer, d.User, d.Name, body)
}

// Rotation keeps the newest backups, one a week for a month, and always the
// newest backup that restored in its check.
const keepWeeks = 4

// RotateBackups deletes the backups the rotation no longer keeps, from R2
// and the list.
func RotateBackups(s *store.Store, dbName string, now time.Time) error {
	all, err := s.ListAllBackups(ctx(), dbName)
	if err != nil {
		return err
	}
	for _, b := range backupsToDrop(all, dbBackupAge, config.BackupKeep, now) {
		if err := deleteParts(s, b.ObjectKey, b.Parts); err != nil {
			return err // retried by the next rotation
		}
		if err := s.DeleteBackup(ctx(), b.ID); err != nil {
			return err
		}
	}
	return nil
}

// deleteParts deletes a backup's parts from the bucket.
func deleteParts(s *store.Store, objectKey string, parts int64) error {
	for i := range int(parts) {
		c, acc, bucket, err := r2(s)
		if err != nil {
			return err
		}
		if err := c.DeleteObject(acc, bucket, partKey(objectKey, i)); err != nil {
			return err
		}
	}
	return nil
}

// dbBackupAge is what the rotation needs to know of a backup: its ID, when
// it was made and whether it restored in its check.
func dbBackupAge(b store.Backup) (id int64, createdAt string, restored bool) {
	return b.ID, b.CreatedAt, b.VerifiedAt != "" && b.VerifyError == ""
}

// backupsToDrop picks what the rotation deletes from backups, newest first.
// Backups still under the bucket's lock can't be deleted, so they're kept.
func backupsToDrop[B any](backups []B, age func(B) (int64, string, bool), keepLast int, now time.Time) []B {
	keep := map[int64]bool{}
	weeks := map[[2]int]bool{}
	cutoff := now.AddDate(0, 0, -7*keepWeeks)
	locked := now.AddDate(0, 0, -backupLockDays)
	verified := false
	for i, b := range backups {
		id, createdAt, restored := age(b)
		if i < keepLast {
			keep[id] = true
		}
		if !verified && restored {
			keep[id], verified = true, true
		}
		t, err := time.Parse("2006-01-02T15:04:05Z", createdAt)
		if err != nil {
			keep[id] = true // unknown age: never guess it's old
			continue
		}
		if t.After(locked) {
			keep[id] = true
		}
		year, week := t.ISOWeek()
		if t.After(cutoff) && !weeks[[2]int{year, week}] {
			keep[id], weeks[[2]int{year, week}] = true, true
		}
	}
	var drop []B
	for _, b := range backups {
		if id, _, _ := age(b); !keep[id] {
			drop = append(drop, b)
		}
	}
	return drop
}

// BackupDue backs up, checks and rotates every database whose last backup
// is older than config.BackupEvery. It's called every hour, so restarts of
// the agent don't postpone backups.
func BackupDue(s *store.Store) map[string]error {
	results := map[string]error{}
	dbs, err := s.ListDatabases(ctx())
	if err != nil || BackupBucket(s) == "" {
		return results
	}
	for _, d := range dbs {
		last, err := s.ListBackups(ctx(), store.ListBackupsParams{Database: d.Name, Limit: 1})
		if err == nil && len(last) > 0 {
			if t, err := time.Parse("2006-01-02T15:04:05Z", last[0].CreatedAt); err == nil && time.Since(t) < config.BackupEvery {
				continue
			}
		}
		results[d.Name] = runDBJob(d.Name, "backing up (scheduled)", func() error { return backupAndCheck(s, d.Name) })
	}
	return results
}
