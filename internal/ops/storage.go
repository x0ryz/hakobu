package ops

import (
	"fmt"
	"strings"

	"github.com/x0ryz/hakobu/internal/s3"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Storages are buckets for the apps' files: R2 in the Cloudflare account
// hakobu is connected to, or any S3-compatible service. hakobu creates an
// R2 storage's bucket itself; the keys the app gets come from an R2 API
// token the owner makes for that bucket alone (hakobu's own token can't
// make one, and its keys would reach every bucket, the backups included).
// Before keys are saved, hakobu checks they reach their bucket and not the
// backups.

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
	case "r2":
		if err := createR2Bucket(s, &st); err != nil {
			return err
		}
	case "s3":
		if st.Endpoint == "" || st.Bucket == "" {
			return fmt.Errorf("endpoint and bucket are required")
		}
		if err := checkStorageKeys(s, st); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported storage provider %q", st.Provider)
	}
	return s.CreateStorage(ctx(), store.CreateStorageParams(st))
}

// createR2Bucket creates the storage's bucket in the connected account. Its
// keys come later (SetStorageKeys), for a token limited to this bucket.
func createR2Bucket(s *store.Store, st *store.Storage) error {
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	if cf.AccountID == "" {
		return fmt.Errorf("finish `hakobu setup` first")
	}
	// Removing a storage keeps its bucket and files: the random part keeps
	// a later storage of the same name, maybe in another project, from
	// being handed them.
	suffix, err := RandomHex(4)
	if err != nil {
		return err
	}
	st.AccountID, st.Bucket = cf.AccountID, "hakobu-"+st.Name+"-"+suffix
	st.AccessKeyID, st.SecretAccessKey = "", ""
	if err := c.CreateBucket(cf.AccountID, st.Bucket); err != nil {
		return fmt.Errorf("creating the R2 bucket: %w", err)
	}
	return nil
}

// SetStorageKeys gives a storage the keys its apps get, once they're known
// to reach its bucket and not the backups.
func SetStorageKeys(s *store.Store, name, accessKeyID, secretAccessKey string) error {
	st, err := s.GetStorage(ctx(), name)
	if err != nil {
		return err
	}
	st.AccessKeyID, st.SecretAccessKey = strings.TrimSpace(accessKeyID), secret.String(strings.TrimSpace(secretAccessKey))
	if err := checkStorageKeys(s, st); err != nil {
		return err
	}
	return s.SetStorageKeys(ctx(), store.SetStorageKeysParams{Name: st.Name, AccessKeyID: st.AccessKeyID, SecretAccessKey: st.SecretAccessKey})
}

// checkStorageKeys makes sure the storage's keys can list its bucket and,
// on R2, can't list the backup bucket: an app with keys to the backups
// could read every database's dumps.
func checkStorageKeys(s *store.Store, st store.Storage) error {
	if st.AccessKeyID == "" || st.SecretAccessKey == "" {
		return fmt.Errorf("access key ID and secret access key are required")
	}
	c := s3.NewClient(st)
	if ok, err := c.CanList(st.Bucket); err != nil {
		return fmt.Errorf("checking the keys: %w", err)
	} else if !ok {
		return fmt.Errorf("these keys can't read bucket %s: give the token Object Read & Write on it", st.Bucket)
	}
	if backups := BackupBucket(s); st.Provider == "r2" && backups != "" && backups != st.Bucket {
		if ok, err := c.CanList(backups); err != nil {
			return fmt.Errorf("checking the keys: %w", err)
		} else if ok {
			return fmt.Errorf("these keys also reach %s, the database backups: make a token for bucket %s alone", backups, st.Bucket)
		}
	}
	return nil
}

func DeleteStorage(s *store.Store, name string) error {
	apps, err := s.AppsUsingStorage(ctx(), name)
	if err != nil {
		return err
	}
	if len(apps) > 0 {
		return fmt.Errorf("storage %s is still used by %s — unlink it first", name, strings.Join(apps, ", "))
	}
	// The bucket and its files stay; delete them at the provider.
	return s.DeleteStorage(ctx(), name)
}

func storageEnv(st store.Storage) []string {
	provider := st.Provider
	if provider == "rustfs" { // made before v0.6: its own endpoint, like S3
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
