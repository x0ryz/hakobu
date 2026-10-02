package backup

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// A volume is copied by a throwaway container that mounts it and runs tar
// (GNU tar from image, which hakobu runs anyway): no network, a read-only
// root, and of root's powers only those that read and write files of any
// owner. Owners are kept by number, as the app's containers see them.
func volumeTar(ctx context.Context, image, volume, mount string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", append([]string{
		"run", "--rm", "-i", "--network", "none", "--read-only",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"-v", volume + ":" + mount, "--entrypoint", "sh", image, "-c",
	}, args...)...)
}

// ArchiveVolume writes a tar of the volume's contents to w as it's made.
func ArchiveVolume(ctx context.Context, image, volume string, w io.Writer) error {
	cmd := volumeTar(ctx, image, volume, "/v:ro", "exec tar --numeric-owner -C /v -cf - .")
	cmd.Stdout = w
	stderr := &tail{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar of %s failed: %w: %s", volume, err, stderr)
	}
	return nil
}

// RestoreVolume replaces everything in the volume with the tar read from r.
func RestoreVolume(ctx context.Context, image, volume string, r io.Reader) error {
	cmd := volumeTar(ctx, image, volume, "/v", "find /v -mindepth 1 -delete && exec tar --numeric-owner -C /v -xpf -")
	cmd.Stdin = r
	stderr := &tail{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restoring %s failed: %w: %s", volume, err, stderr)
	}
	return nil
}
