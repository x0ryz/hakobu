package build

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/x0ryz/hakobu/internal/config"
)

func BuildWithStrategy(sourceDir, imageTag, strategy string, out io.Writer) error {
	if strategy == "dockerfile" {
		return run(out, "docker", "build", "-t", imageTag, sourceDir)
	}
	ensureBuildKit(out)
	if err := run(out, "railpack", "build", sourceDir, "--name", imageTag); err != nil {
		return fmt.Errorf("railpack build failed (need railpack + buildkit, see https://railpack.com): %w", err)
	}
	return nil
}

func run(out io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// ensureBuildKit points railpack at the "buildkit" container install.sh
// starts, starting it if missing, unless BUILDKIT_HOST is already set.
func ensureBuildKit(out io.Writer) {
	if os.Getenv("BUILDKIT_HOST") != "" {
		return
	}
	if exec.Command("docker", "inspect", "buildkit").Run() != nil {
		fmt.Fprintln(out, "starting buildkit container for railpack...")
		if exec.Command("docker", "run", "--privileged", "-d", "--restart", "unless-stopped", "--name", "buildkit", config.BuildKitImage).Run() != nil {
			return
		}
	}
	os.Setenv("BUILDKIT_HOST", "docker-container://buildkit")
}
