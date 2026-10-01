package backup

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Dumps are in pg_dump's custom format and replayed with pg_restore, not
// psql: a plain SQL dump can hold psql meta-commands (\! runs a shell in
// the Postgres container, \c switches to the superuser over the trusted
// local socket), so a tampered dump could take over every database. The
// worst a tampered custom-format dump can do is run SQL as the database's
// own user. The tools run as the image's postgres user, not root.

// DumpDatabase writes a compressed custom-format pg_dump to w as it's
// produced. It uses the docker CLI so stdout (the dump) and stderr stay
// separate without demuxing the exec stream.
func DumpDatabase(ctx context.Context, containerName, dbUser, dbName string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-u", "postgres", containerName, "pg_dump", "-Fc", "-U", dbUser, "-d", dbName)
	cmd.Stdout = w
	stderr := &tail{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w: %s", err, stderr)
	}
	return nil
}

// RestoreDatabase replays a dump into dbName as dbUser, in one transaction
// that stops at the first error: it either fully applies or changes
// nothing.
func RestoreDatabase(ctx context.Context, containerName, dbUser, dbName string, dump io.Reader) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-u", "postgres", containerName, "pg_restore", "-U", dbUser, "-d", dbName, "--single-transaction", "--exit-on-error")
	cmd.Stdin = dump
	stderr := &tail{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, stderr)
	}
	return nil
}

// CountTables counts the user tables in dbName, a sanity check that a
// restored dump actually held data.
func CountTables(ctx context.Context, containerName, dbUser, dbName string) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "postgres", containerName, "psql", "-U", dbUser, "-d", dbName, "-Atc",
		"SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("psql: %w: %s", err, out)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// tail keeps the last 4 KB written to it, enough for an error message from
// a command that may print a lot before failing.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.b)) }
