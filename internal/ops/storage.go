package ops

import (
	"fmt"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/s3"
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

// provisionRustFS starts the RustFS container on first use, creates the
// storage's bucket and a RustFS user that can reach only that bucket, so an
// app linked to one storage can't read another project's files. The root
// keys stay in the container's environment, for hakobu alone.
func provisionRustFS(st *store.Storage) error {
	st.Endpoint = "http://" + rustfsContainer + ":" + rustfsPort
	st.Bucket = "hakobu-" + st.Name
	root, err := rustfsRoot(st.Bucket)
	if err != nil {
		return err
	}
	// RustFS answers 503 until its storage is up, so the first successful
	// request is the bucket itself.
	if !deploy.WaitHealthy(60, time.Second, func() bool { err = root.CreateBucket(); return err == nil }) {
		return fmt.Errorf("rustfs failed to become ready: %w", err)
	}
	// MinIO-style limits: access keys up to 20 characters, secrets up to 40.
	if st.AccessKeyID, err = RandomHex(9); err != nil {
		return err
	}
	st.AccessKeyID = "hk" + st.AccessKeyID
	key, err := RandomHex(20)
	if err != nil {
		return err
	}
	st.SecretAccessKey = secret.String(key)
	return root.AddBucketUser(st.AccessKeyID, key)
}

// rustfsRoot returns a client with RustFS's root keys for bucket, starting
// the container (and choosing the keys) if needed. RustFS is only reachable
// by container name inside docker, so the client goes via its IP.
func rustfsRoot(bucket string) (*s3.Client, error) {
	env, err := deploy.ContainerEnv(ctx(), rustfsContainer)
	if err != nil {
		return nil, err
	}
	if env != nil {
		err = deploy.StartContainer(ctx(), rustfsContainer)
	} else {
		env = map[string]string{"RUSTFS_ADDRESS": ":" + rustfsPort, "RUSTFS_CONSOLE_ENABLE": "false"}
		if env["RUSTFS_ACCESS_KEY"], err = RandomHex(16); err != nil {
			return nil, err
		}
		if env["RUSTFS_SECRET_KEY"], err = RandomHex(32); err != nil {
			return nil, err
		}
		var list []string
		for k, v := range env {
			list = append(list, k+"="+v)
		}
		_, err = deploy.RunServiceContainer(ctx(), rustfsContainer, "rustfs/rustfs:latest", list, "/data")
	}
	if err != nil {
		return nil, err
	}
	ip, err := deploy.ContainerIP(ctx(), rustfsContainer)
	if err != nil {
		return nil, err
	}
	return s3.NewClient(store.Storage{
		Endpoint: "http://" + ip + ":" + rustfsPort, Bucket: bucket,
		AccessKeyID: env["RUSTFS_ACCESS_KEY"], SecretAccessKey: secret.String(env["RUSTFS_SECRET_KEY"]),
	}), nil
}

func DeleteStorage(s *store.Store, name string) error {
	apps, err := s.AppsUsingStorage(ctx(), name)
	if err != nil {
		return err
	}
	if len(apps) > 0 {
		return fmt.Errorf("storage %s is still used by %s — unlink it first", name, strings.Join(apps, ", "))
	}
	st, err := s.GetStorage(ctx(), name)
	if err != nil {
		return err
	}
	// The bucket and its files stay, as with R2 and S3; only the keys go.
	if st.Provider == "rustfs" {
		root, err := rustfsRoot(st.Bucket)
		if err != nil {
			return err
		}
		if err := root.RemoveBucketUser(st.AccessKeyID); err != nil {
			return err
		}
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
	return append(env, "S3_ENDPOINT="+s3.Endpoint(st))
}
