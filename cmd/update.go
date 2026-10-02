package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/update"
)

var (
	updateTo        string
	updateRequested bool
)

// updateCmd installs a newer signed release; the panel's Update button
// gets systemd to run it with --requested.
var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update hakobu to its latest signed release (as root)",
	Long: `Downloads the latest release, checks its signature and checksum, copies the
panel's database, swaps the binary and restarts hakobu; apps keep running. If the
new version doesn't come up, the previous one is put back. hakobu rollback goes
back to it later.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := installDir()
		if err != nil {
			return err
		}
		if updateRequested {
			// Removed first: a request that fails isn't retried in a loop.
			// Whatever the panel wrote in it is never read.
			if updateTo != "" {
				return errors.New("--requested installs the latest release only")
			}
			if err := os.Remove(filepath.Join(dir, update.RequestFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		in, err := update.Detect(dir, version, config.AgentAddr, os.Stdout)
		if err != nil {
			return err
		}
		return in.Update(updateTo)
	},
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Go back to the hakobu from before the last update (as root)",
	Long: `Puts back the binary hakobu ran before its last update. If that update changed
the panel's database, the copy made just before it is put back too (what changed
in the panel since is lost; the newer database is kept beside it).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := installDir()
		if err != nil {
			return err
		}
		in, err := update.Detect(dir, version, config.AgentAddr, os.Stdout)
		if err != nil {
			return err
		}
		return in.Rollback()
	},
}

// snapshotDBCmd and schemaVersionCmd are run by update and rollback, as
// the user hakobu runs as, so root never opens hakobu's data.
var snapshotDBCmd = &cobra.Command{
	Use:    "snapshot-db <copy>",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.PrepareDataDir(); err != nil {
			return err
		}
		if err := os.Remove(args[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return store.CopyDatabase(config.DatabaseFile, args[0])
	},
}

var schemaVersionCmd = &cobra.Command{
	Use:    "schema-version [database]",
	Hidden: true,
	Args:   cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			fmt.Println(store.SchemaVersion())
			return nil
		}
		v, err := store.DatabaseVersion(args[0])
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	},
}

// installDir is where the running binary is installed, /opt/hakobu.
func installDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func init() {
	updateCmd.Flags().StringVar(&updateTo, "to", "", "install this release (e.g. v1.2.3) instead of the latest; it must be signed too")
	updateCmd.Flags().BoolVar(&updateRequested, "requested", false, "run for the panel's Update button (by systemd)")
	rootCmd.AddCommand(updateCmd, rollbackCmd, snapshotDBCmd, schemaVersionCmd)
}
