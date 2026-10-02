package ops

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Volumes are backed up daily like databases: a tar of the volume, sealed,
// to the backup bucket, read back right after to prove it's whole. The
// containers using the volume are paused while tar reads it, so files
// written together (an SQLite database and its journal) are copied as of
// one moment, as after a power cut; an app is frozen for as long as that
// takes, seconds for most volumes.

// volumeJob names a volume's backup jobs (DatabaseJob): volume and
// database names hold no "/", so they never clash.
func volumeJob(app, volume string) string { return app + "/" + volume }

// VolumeJob is the running or last backup job of an app's volume.
func VolumeJob(app, volume string) DBJob { return DatabaseJob(volumeJob(app, volume)) }

// errAppBusy is a volume backup put off because the app is being deployed.
var errAppBusy = errors.New("a deploy is in progress")

// BackupVolume uploads a backup of the volume; it returns errAppBusy while
// the app is deploying, and 0 for a volume no deploy has created yet.
func BackupVolume(s *store.Store, app, volume string) (int64, error) {
	if _, err := s.GetApp(ctx(), app); err != nil {
		return 0, err
	}
	name := dockerVolume(app, volume)
	if ok, err := deploy.VolumeExists(ctx(), name); err != nil || !ok {
		return 0, err
	}
	key := fmt.Sprintf("volumes/%s_%s/%s.tar.enc", app, volume, time.Now().UTC().Format("20060102-150405"))
	obj, err := uploadSealed(s, key, func(w io.Writer) error {
		// Held like a deploy while the app is paused, so no deploy starts
		// or stops its containers meanwhile.
		if ok, _ := reserve(app, false); !ok {
			return errAppBusy
		}
		defer func() {
			if release(app) {
				if err := StartDeploy(s, app, "push"); err != nil {
					fmt.Println("deploying", app, "after its backup failed:", err)
				}
			}
		}()
		paused := pauseUsers(name)
		defer unpause(paused)
		return backup.ArchiveVolume(ctx(), config.PostgresImage, name, w)
	})
	if err != nil {
		return 0, err
	}
	return s.CreateVolumeBackup(ctx(), store.CreateVolumeBackupParams{
		AppName: app, Volume: volume, ObjectKey: key, Parts: int64(obj.parts), SizeBytes: obj.size, SHA256: obj.sha256, FileKey: secret.String(obj.fileKey),
	})
}

// pauseUsers pauses the running containers that mount volume and returns
// them. One that can't be paused is copied running: a backup that may
// need the app's own recovery beats none.
func pauseUsers(volume string) []string {
	users, err := deploy.RunningWithVolume(ctx(), volume)
	if err != nil {
		fmt.Println("backup of", volume+": copying without pausing its containers:", err)
		return nil
	}
	var paused []string
	for _, c := range users {
		if err := deploy.PauseContainer(ctx(), c); err != nil {
			fmt.Println("backup of", volume+": copying while", c, "runs:", err)
			continue
		}
		paused = append(paused, c)
	}
	return paused
}

func unpause(containers []string) {
	for _, c := range containers {
		if err := deploy.UnpauseContainer(ctx(), c); err != nil {
			fmt.Println("failed to unpause", c+":", err)
		}
	}
}

// fetchVolumeBackup is fetchSealed for a volume backup.
func fetchVolumeBackup(s *store.Store, b store.VolumeBackup) (io.ReadCloser, error) {
	return fetchSealed(s, b.ObjectKey, b.Parts, b.SHA256, string(b.FileKey))
}

// VerifyVolumeBackup downloads a backup and reads the whole tar in it; the
// result is saved on the backup.
func VerifyVolumeBackup(s *store.Store, id int64) error {
	b, err := s.GetVolumeBackup(ctx(), id)
	if err != nil {
		return err
	}
	files, verifyErr := countTarFiles(s, b)
	msg := ""
	if verifyErr != nil {
		msg = verifyErr.Error()
	}
	if err := s.SetVolumeBackupVerified(ctx(), store.SetVolumeBackupVerifiedParams{
		ID: id, VerifiedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), VerifyError: msg, Files: int64(files),
	}); err != nil {
		return err
	}
	return verifyErr
}

func countTarFiles(s *store.Store, b store.VolumeBackup) (int, error) {
	body, err := fetchVolumeBackup(s, b)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	tr := tar.NewReader(body)
	files := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("the tar is broken: %w", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil { // reads (and authenticates) every byte
			return files, err
		}
		if h.Typeflag != tar.TypeDir {
			files++
		}
	}
}

// volumeBackupAndCheck backs a volume up, reads the backup back and drops
// old ones.
func volumeBackupAndCheck(s *store.Store, app, volume string) error {
	id, err := BackupVolume(s, app, volume)
	if err != nil || id == 0 {
		return err
	}
	verifyErr := VerifyVolumeBackup(s, id)
	if err := RotateVolumeBackups(s, app, volume, time.Now()); err != nil {
		fmt.Println("backup rotation of", volumeJob(app, volume), "failed:", err)
	}
	if verifyErr != nil {
		return fmt.Errorf("the backup was uploaded, but reading it back failed: %w", verifyErr)
	}
	return nil
}

// StartVolumeBackup backs the volume up in the background.
func StartVolumeBackup(s *store.Store, app, volume string) error {
	if err := hasVolume(s, app, volume); err != nil {
		return err
	}
	return startDBJob(volumeJob(app, volume), "backing up", func() error {
		err := volumeBackupAndCheck(s, app, volume)
		if errors.Is(err, errAppBusy) {
			return fmt.Errorf("%w, try again when it finishes", err)
		}
		return err
	})
}

// StartVolumeVerify reads a backup back in the background.
func StartVolumeVerify(s *store.Store, app, volume string, id int64) error {
	b, err := volumeBackup(s, app, volume, id)
	if err != nil {
		return err
	}
	return startDBJob(volumeJob(app, volume), "checking a backup", func() error { return VerifyVolumeBackup(s, b.ID) })
}

// StartVolumeRestore replaces the volume's contents with a backup, as a
// job in the app's deploy history: the app and its worker are stopped
// while it's written and started again after.
func StartVolumeRestore(s *store.Store, app, volume string, id int64) error {
	b, err := volumeBackup(s, app, volume, id)
	if err != nil {
		return err
	}
	job := volumeJob(app, volume)
	what := "restoring the backup of " + b.CreatedAt
	if err := reserveDB(job, what); err != nil {
		return err
	}
	err = startJob(s, app, "restore volume "+volume, func(a store.App, out io.Writer) error {
		err := restoreVolume(s, a, b, out)
		finishDB(job, what, err)
		return err
	})
	if err != nil {
		finishDB(job, what, err)
	}
	return err
}

func restoreVolume(s *store.Store, app store.App, b store.VolumeBackup, out io.Writer) error {
	fmt.Fprintln(out, "downloading the backup of", b.CreatedAt)
	body, err := fetchVolumeBackup(s, b) // checked whole before anything is stopped
	if err != nil {
		return err
	}
	defer body.Close()
	fmt.Fprintln(out, "stopping", app.Name)
	if err := stopApp(app); err != nil {
		return err
	}
	defer restartWorker(s, app, out)
	defer restartContainer(app, app.ContainerName(), out)
	fmt.Fprintln(out, "replacing the contents of volume", b.Volume)
	if err := backup.RestoreVolume(ctx(), config.PostgresImage, dockerVolume(app.Name, b.Volume), body); err != nil {
		return err
	}
	fmt.Fprintln(out, "restored; starting", app.Name, "again")
	return nil
}

func hasVolume(s *store.Store, app, volume string) error {
	vols, err := s.ListVolumes(ctx(), app)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name == volume {
			return nil
		}
	}
	return fmt.Errorf("%s has no volume %s", app, volume)
}

// volumeBackup looks up a backup and makes sure it's of app's volume.
func volumeBackup(s *store.Store, app, volume string, id int64) (store.VolumeBackup, error) {
	b, err := s.GetVolumeBackup(ctx(), id)
	if err != nil || b.AppName != app || b.Volume != volume {
		return b, fmt.Errorf("no such backup of %s", volumeJob(app, volume))
	}
	return b, hasVolume(s, app, volume)
}

func volumeBackupAge(b store.VolumeBackup) (int64, string, bool) {
	return b.ID, b.CreatedAt, b.VerifiedAt != "" && b.VerifyError == ""
}

// RotateVolumeBackups deletes the volume's backups the rotation no longer
// keeps, the same as a database's.
func RotateVolumeBackups(s *store.Store, app, volume string, now time.Time) error {
	all, err := s.ListAllVolumeBackups(ctx(), store.ListAllVolumeBackupsParams{AppName: app, Volume: volume})
	if err != nil {
		return err
	}
	for _, b := range backupsToDrop(all, volumeBackupAge, config.BackupKeep, now) {
		if err := deleteParts(s, b.ObjectKey, b.Parts); err != nil {
			return err // retried by the next rotation
		}
		if err := s.DeleteVolumeBackup(ctx(), b.ID); err != nil {
			return err
		}
	}
	return nil
}

// VolumeBackupDue backs up every volume whose last backup is older than
// config.BackupEvery, keyed by "app/volume" like NoteBackup wants them. A
// volume of an app being deployed waits for the next hour.
func VolumeBackupDue(s *store.Store) map[string]error {
	results := map[string]error{}
	vols, err := s.ListAllVolumes(ctx())
	if err != nil || BackupBucket(s) == "" {
		return results
	}
	for _, v := range vols {
		last, err := s.ListVolumeBackups(ctx(), store.ListVolumeBackupsParams{AppName: v.AppName, Volume: v.Name, Limit: 1})
		if err == nil && len(last) > 0 {
			if t, err := time.Parse("2006-01-02T15:04:05Z", last[0].CreatedAt); err == nil && time.Since(t) < config.BackupEvery {
				continue
			}
		}
		err = runDBJob(volumeJob(v.AppName, v.Name), "backing up (scheduled)", func() error { return volumeBackupAndCheck(s, v.AppName, v.Name) })
		if !errors.Is(err, errAppBusy) {
			results[volumeJob(v.AppName, v.Name)] = err
		}
	}
	return results
}
