package ops

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Snapshots: before each deploy the app's database is dumped to the local
// disk, next to the :previous image, so Rollback can return the data a bad
// migration broke together with the code. It's an undo, not a backup: it
// lives on this server and only the last one is kept. Dumps are sealed
// with the master key (secret.NewFileWriter): a copy of data/ without the
// key doesn't give away the apps' data.

const (
	snapshotDir = "data/snapshots"
	snapshotExt = ".sql.gz.enc"
)

func snapshotPath(app string) string { return filepath.Join(snapshotDir, app+snapshotExt) }

// dumpTo writes a sealed, gzipped dump of d to a new file in snapshotDir.
func dumpTo(d store.Database) (path string, err error) {
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(snapshotDir, "dump-*.tmp")
	if err != nil {
		return "", err
	}
	defer f.Close()
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	w, err := secret.NewFileWriter(f)
	if err != nil {
		return "", err
	}
	if err := backup.DumpDatabase(ctx(), PostgresContainer, d.User, d.Name, w); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return f.Name(), f.Sync()
}

// takeSnapshot dumps the app's database before a deploy; "" if the app has
// none or the dump failed (the deploy goes on without one).
func takeSnapshot(s *store.Store, app store.App, out io.Writer) string {
	if app.LinkedDB == "" {
		return ""
	}
	d, err := s.GetDatabase(ctx(), app.LinkedDB)
	if err == nil {
		fmt.Fprintln(out, "saving a snapshot of database", d.Name, "for rollback")
		var path string
		if path, err = dumpTo(d); err == nil {
			return path
		}
	}
	fmt.Fprintln(out, "warning: no database snapshot, Rollback will only restore the code:", err)
	return ""
}

// keepSnapshot makes a dump the app's snapshot of db, or with dump "" drops
// the snapshot: it must always belong to the :previous image.
func keepSnapshot(s *store.Store, app, db, dump string) error {
	if dump == "" {
		os.Remove(snapshotPath(app))
		return s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{Name: app})
	}
	if err := os.Rename(dump, snapshotPath(app)); err != nil {
		return err
	}
	return s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{Name: app, SnapshotDB: db, SnapshotAt: time.Now().UTC().Format("2006-01-02T15:04:05Z")})
}

// DataRollbackBlocker says why Rollback can't return the app's data, ""
// if it can.
func DataRollbackBlocker(s *store.Store, app store.App) string {
	switch {
	case app.SnapshotDB == "":
		return "there's no database snapshot from before the last deploy"
	case app.SnapshotDB != app.LinkedDB:
		return "the snapshot is of " + app.SnapshotDB + ", which isn't the app's database any more"
	}
	if _, err := os.Stat(snapshotPath(app.Name)); err != nil {
		return "the snapshot file is missing"
	}
	if users, err := s.AppsUsingDatabase(ctx(), app.LinkedDB); err != nil || len(users) != 1 {
		return "other apps use " + app.LinkedDB + " too, and would lose their data as well"
	}
	return ""
}

// replaceDatabase swaps d's content for a dump: the dump is restored into a
// scratch database first, so a broken one changes nothing, then renamed
// over d. Nothing may be connected to d.
func replaceDatabase(d store.Database, dump string) error {
	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := secret.NewFileReader(f)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(dump), err)
	}
	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.restore." + d.Name
	drop := fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, scratch)
	for _, sql := range []string{
		drop,
		fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, scratch, d.User),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM PUBLIC`, scratch),
	} {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, sql); err != nil {
			return err
		}
	}
	if err := backup.RestoreDatabase(ctx(), PostgresContainer, d.User, scratch, r); err != nil {
		if derr := deploy.PostgresExec(ctx(), PostgresContainer, drop); derr != nil {
			fmt.Println("failed to drop", scratch+":", derr)
		}
		return err
	}
	for _, sql := range []string{
		fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, d.Name),
		fmt.Sprintf(`ALTER DATABASE "%s" RENAME TO "%s"`, scratch, d.Name),
	} {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, sql); err != nil {
			return err
		}
	}
	return nil
}
