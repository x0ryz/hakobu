package ops

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

func SetBackupStorage(s *store.Store, dbName, storageName string) error {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return err
	}
	if storageName != "" {
		st, err := s.GetStorage(ctx(), storageName)
		if err != nil || st.ProjectID != d.ProjectID {
			return fmt.Errorf("storage %q not found in this project", storageName)
		}
	}
	return s.SetDatabaseBackupStorage(ctx(), store.SetDatabaseBackupStorageParams{Name: dbName, BackupStorage: storageName})
}

var errNoStorage = errors.New("the storage no longer exists")

// storageClient returns a client for a backup's storage; "" is the
// database's current backup storage.
func storageClient(s *store.Store, d store.Database, storage string) (*backup.Client, error) {
	if storage == "" {
		storage = d.BackupStorage
	}
	if storage == "" {
		return nil, fmt.Errorf("pick a backup storage for %s first", d.Name)
	}
	st, err := s.GetStorage(ctx(), storage)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", storage, errNoStorage)
	}
	return hostClient(st)
}

// SameServer reports whether a storage lives on this server, which makes
// backups to it lost together with the database if the server's disk is.
func SameServer(st store.Storage) bool { return st.Provider == "rustfs" }

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

// BackupDatabase streams a gzipped pg_dump through a temporary file (S3
// needs the size and hash up front) to the database's backup storage.
func BackupDatabase(s *store.Store, dbName string) (id int64, err error) {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return 0, err
	}
	client, err := storageClient(s, d, "")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(tmpDir, "backup-*.sql.gz")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := backup.DumpDatabase(ctx(), PostgresContainer, d.User, d.Name, f); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("backups/%s/%s.sql.gz", d.Name, time.Now().UTC().Format("20060102-150405"))
	if err := client.PutFile(key, f, "application/gzip"); err != nil {
		return 0, err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{Database: d.Name, Storage: d.BackupStorage, ObjectKey: key, SizeBytes: info.Size()})
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
	client, err := storageClient(s, d, b.Storage)
	if err != nil {
		return 0, err
	}
	body, err := client.GetStream(b.ObjectKey)
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
	defer deploy.PostgresExec(ctx(), PostgresContainer, drop)
	if err := backup.RestoreDatabase(ctx(), PostgresContainer, d.User, scratch, body); err != nil {
		return 0, err
	}
	return backup.CountTables(ctx(), PostgresContainer, d.User, scratch)
}

// restoreBackup replays a backup into its database. The dump is plain SQL,
// so rows that already exist cause errors rather than being overwritten.
func restoreBackup(s *store.Store, b store.Backup) error {
	d, err := s.GetDatabase(ctx(), b.Database)
	if err != nil {
		return err
	}
	client, err := storageClient(s, d, b.Storage)
	if err != nil {
		return err
	}
	body, err := client.GetStream(b.ObjectKey)
	if err != nil {
		return err
	}
	defer body.Close()
	return backup.RestoreDatabase(ctx(), PostgresContainer, d.User, d.Name, body)
}

// Rotation keeps the newest backups, one a week for a month, and always the
// newest backup that restored in its check.
const keepWeeks = 4

// RotateBackups deletes the backups the rotation no longer keeps, from the
// storage and the list.
func RotateBackups(s *store.Store, dbName string, now time.Time) error {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return err
	}
	all, err := s.ListAllBackups(ctx(), dbName)
	if err != nil {
		return err
	}
	for _, b := range backupsToDrop(all, config.BackupKeep, now) {
		// A backup on a storage that's gone is only forgotten; any other
		// failure is retried by the next rotation.
		client, err := storageClient(s, d, b.Storage)
		if err == nil {
			err = client.DeleteObject(b.ObjectKey)
		}
		if err != nil && !errors.Is(err, errNoStorage) {
			return err
		}
		if err := s.DeleteBackup(ctx(), b.ID); err != nil {
			return err
		}
	}
	return nil
}

// backupsToDrop picks what the rotation deletes from backups, newest first.
func backupsToDrop(backups []store.Backup, keepLast int, now time.Time) []store.Backup {
	keep := map[int64]bool{}
	weeks := map[[2]int]bool{}
	cutoff := now.AddDate(0, 0, -7*keepWeeks)
	verified := false
	for i, b := range backups {
		if i < keepLast {
			keep[b.ID] = true
		}
		if !verified && b.VerifiedAt != "" && b.VerifyError == "" {
			keep[b.ID], verified = true, true
		}
		t, err := time.Parse("2006-01-02T15:04:05Z", b.CreatedAt)
		if err != nil {
			keep[b.ID] = true // unknown age: never guess it's old
			continue
		}
		year, week := t.ISOWeek()
		if t.After(cutoff) && !weeks[[2]int{year, week}] {
			keep[b.ID], weeks[[2]int{year, week}] = true, true
		}
	}
	var drop []store.Backup
	for _, b := range backups {
		if !keep[b.ID] {
			drop = append(drop, b)
		}
	}
	return drop
}

// BackupDue backs up, checks and rotates every database with a backup
// storage whose last backup is older than config.BackupEvery. It's called
// every hour, so restarts of the agent don't postpone backups.
func BackupDue(s *store.Store) map[string]error {
	results := map[string]error{}
	dbs, err := s.ListDatabases(ctx())
	if err != nil {
		return results
	}
	for _, d := range dbs {
		if d.BackupStorage == "" {
			continue
		}
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
