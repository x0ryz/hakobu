package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// DumpDatabase writes a gzipped plain-SQL pg_dump to w as it's produced. It
// uses the docker CLI so stdout (the dump) and stderr stay separate without
// demuxing the exec stream.
func DumpDatabase(ctx context.Context, containerName, dbUser, dbName string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", containerName, "pg_dump", "-U", dbUser, "-d", dbName)
	gz := gzip.NewWriter(w)
	cmd.Stdout = gz
	stderr := &tail{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w: %s", err, stderr)
	}
	return gz.Close()
}

// RestoreDatabase replays a gzipped dump into dbName as dbUser, in one
// transaction that stops at the first error: it either fully applies or
// changes nothing.
func RestoreDatabase(ctx context.Context, containerName, dbUser, dbName string, gzipDump io.Reader) error {
	r, err := gzip.NewReader(gzipDump)
	if err != nil {
		return fmt.Errorf("invalid backup archive: %w", err)
	}
	defer r.Close()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "psql", "-q", "-U", dbUser, "-d", dbName, "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Stdin = r
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
	out, err := exec.CommandContext(ctx, "docker", "exec", containerName, "psql", "-U", dbUser, "-d", dbName, "-Atc",
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
