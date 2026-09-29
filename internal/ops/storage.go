package ops

import (
	"fmt"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// RustFS is the optional self-hosted S3 service, one container shared by all
// "rustfs" storages (one bucket each).
const (
	rustfsContainer = "hakobu-rustfs"
	rustfsPort      = "9000"
)

func CreateStorage(s *store.Store, projectName string, st store.Storage) error {
	if err := checkName("storage", st.Name); err != nil {
		return err
	}
	p, err := s.GetProject(ctx(), projectName)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if _, err := s.GetStorage(ctx(), st.Name); err == nil {
		return fmt.Errorf("a storage named %q already exists", st.Name)
	}
	st.ProjectID = p.ID
	if st.Region == "" {
		st.Region = "auto"
	}
	switch st.Provider {
	case "rustfs":
		if err := provisionRustFS(&st); err != nil {
			return err
		}
	case "r2", "s3":
		if st.AccessKeyID == "" || st.SecretAccessKey == "" || st.Bucket == "" {
			return fmt.Errorf("access key, secret key and bucket are required")
		}
	default:
		return fmt.Errorf("unsupported storage provider %q", st.Provider)
	}
	return s.CreateStorage(ctx(), store.CreateStorageParams(st))
}

// provisionRustFS starts the RustFS container on first use, reusing an
// existing one and its keys, and creates the storage's bucket.
func provisionRustFS(st *store.Storage) error {
	st.Endpoint = "http://" + rustfsContainer + ":" + rustfsPort
	st.Bucket = "hakobu-" + st.Name

	env, err := deploy.ContainerEnv(ctx(), rustfsContainer)
	if err != nil {
		return err
	}
	if env != nil {
		st.AccessKeyID, st.SecretAccessKey = env["RUSTFS_ACCESS_KEY"], secret.String(env["RUSTFS_SECRET_KEY"])
		if err := deploy.StartContainer(ctx(), rustfsContainer); err != nil {
			return err
		}
	} else {
		if st.AccessKeyID, err = RandomHex(16); err != nil {
			return err
		}
		key, err := RandomHex(32)
		if err != nil {
			return err
		}
		st.SecretAccessKey = secret.String(key)
		env := []string{
			"RUSTFS_ACCESS_KEY=" + st.AccessKeyID,
			"RUSTFS_SECRET_KEY=" + key,
			"RUSTFS_ADDRESS=:" + rustfsPort,
			"RUSTFS_CONSOLE_ENABLE=false",
		}
		if _, err := deploy.RunServiceContainer(ctx(), rustfsContainer, "rustfs/rustfs:latest", env, "/data"); err != nil {
			return err
		}
	}

	client, err := hostClient(*st)
	if err != nil {
		return err
	}
	// RustFS answers 503 until its storage is up, so the first successful
	// request is the bucket itself.
	if !deploy.WaitHealthy(60, time.Second, func() bool { err = client.CreateBucket(); return err == nil }) {
		return fmt.Errorf("rustfs failed to become ready: %w", err)
	}
	return nil
}

// hostClient returns an S3 client usable from the agent process. RustFS is
// only reachable by container name inside docker, so it goes via its IP.
func hostClient(st store.Storage) (*backup.Client, error) {
	if st.Provider == "rustfs" {
		ip, err := deploy.ContainerIP(ctx(), rustfsContainer)
		if err != nil {
			return nil, err
		}
		st.Endpoint = "http://" + ip + ":" + rustfsPort
	}
	return backup.NewClient(st), nil
}

func DeleteStorage(s *store.Store, name string) error {
	apps, err := s.AppsUsingStorage(ctx(), name)
	if err != nil {
		return err
	}
	if len(apps) > 0 {
		return fmt.Errorf("storage %s is still used by %s — unlink it first", name, strings.Join(apps, ", "))
	}
	dbs, err := s.DatabasesBackingUpTo(ctx(), name)
	if err != nil {
		return err
	}
	if len(dbs) > 0 {
		return fmt.Errorf("storage %s holds backups of %s — pick another backup storage first", name, strings.Join(dbs, ", "))
	}
	return s.DeleteStorage(ctx(), name)
}

func storageEnv(st store.Storage) []string {
	provider := st.Provider
	if provider == "rustfs" {
		provider = "s3"
	}
	env := []string{
		"STORAGE_PROVIDER=" + provider,
		"S3_ACCESS_KEY_ID=" + st.AccessKeyID,
		"S3_SECRET_ACCESS_KEY=" + string(st.SecretAccessKey),
		"S3_BUCKET_NAME=" + st.Bucket,
		"S3_REGION=" + st.Region,
	}
	if st.Provider == "r2" {
		env = append(env, "R2_ACCOUNT_ID="+st.AccountID)
	}
	return append(env, "S3_ENDPOINT="+backup.Endpoint(st))
}
