package update

import (
	"fmt"
	"os"
	"path/filepath"
)

// The panel's Update and Roll back buttons: hakobu, which can't write its
// own binary, creates a request file in data/, and systemd runs the job as
// root. Neither job reads the request: an update installs the latest
// signed release, a rollback the version that ran before the last update.
// Each version installs its own units (InstallUnits), so a new button
// works right after the update that brings it.

// RollbackRequestFile, under the install's directory, asks for a rollback.
const RollbackRequestFile = "data/rollback-request"

var unitDir = "/etc/systemd/system"

func units(dir string) map[string]string {
	bin := filepath.Join(dir, binaryFile)
	return map[string]string{
		"hakobu-update.path": fmt.Sprintf(`[Unit]
Description=hakobu updates asked for in its panel

[Path]
PathExists=%s

[Install]
WantedBy=multi-user.target
`, filepath.Join(dir, RequestFile)),
		"hakobu-update.service": fmt.Sprintf(`[Unit]
Description=Update hakobu to its latest signed release

[Service]
Type=oneshot
WorkingDirectory=%s
ExecStart=%s update --requested
TimeoutStartSec=20min
`, dir, bin),
		"hakobu-rollback.path": fmt.Sprintf(`[Unit]
Description=hakobu rollbacks asked for in its panel

[Path]
PathExists=%s

[Install]
WantedBy=multi-user.target
`, filepath.Join(dir, RollbackRequestFile)),
		"hakobu-rollback.service": fmt.Sprintf(`[Unit]
Description=Put back the hakobu from before its last update

[Service]
Type=oneshot
WorkingDirectory=%s
ExecStart=%s rollback --requested
TimeoutStartSec=20min
`, dir, bin),
	}
}

// InstallUnits writes the updater's systemd units and starts watching for
// requests. It runs as root, from install.sh and after an update.
func (in *Install) InstallUnits() error {
	for name, body := range units(in.Dir) {
		if err := writeFileAtomic(filepath.Join(unitDir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	if err := in.systemctl("daemon-reload"); err != nil {
		return err
	}
	return in.systemctl("enable", "--now", "hakobu-update.path", "hakobu-rollback.path")
}

// UnitsInstalled reports whether the panel's requests are picked up: the
// Update button needs hakobu-update.path, Roll back hakobu-rollback.path.
func UnitsInstalled(name string) bool {
	_, err := os.Stat(filepath.Join(unitDir, name))
	return err == nil
}
