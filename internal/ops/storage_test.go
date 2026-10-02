package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/s3"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestR2Storages(t *testing.T) {
	var buckets []string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/accounts/acc/r2/buckets" {
			var body struct{ Name string }
			_ = jsonDecode(r, &body)
			buckets = append(buckets, body.Name)
			fmt.Fprint(w, `{"success":true,"result":{}}`)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer cf.Close()
	oldCF := cloudflare.APIURL
	cloudflare.APIURL = cf.URL
	defer func() { cloudflare.APIURL = oldCF }()

	// R2's S3 API: "narrow" keys list the storage's bucket only, "wide" ones
	// every bucket, "none" nothing.
	r2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "Credential=wide/"),
			strings.Contains(auth, "Credential=narrow/") && strings.Contains(r.URL.Path, "/hakobu-files-"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer r2.Close()
	oldR2 := s3.R2Endpoint
	s3.R2Endpoint = r2.URL + "/%s"
	defer func() { s3.R2Endpoint = oldR2 }()

	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(s.SetBackupBucket(ctx(), "hakobu-backups-1"))
	must(s.CreateProject(ctx(), "shop"))
	p, _ := s.GetProject(ctx(), "shop")
	must(s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}))

	// The bucket is made in the account; keys come later.
	must(CreateStorage(s, "shop", store.Storage{Name: "files", Provider: "r2"}))
	st, _ := s.GetStorage(ctx(), "files")
	if len(buckets) != 1 || buckets[0] != st.Bucket || !strings.HasPrefix(st.Bucket, "hakobu-files-") || st.AccountID != "acc" || st.AccessKeyID != "" {
		t.Fatalf("storage %+v, buckets made %v", st, buckets)
	}
	if err := LinkStorage(s, "web", "files"); err == nil {
		t.Error("linked a storage without keys")
	}

	for keys, want := range map[string]string{"none": "can't read", "wide": "backups"} {
		if err := SetStorageKeys(s, "files", keys, "secret"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s keys: %v, want an error about %q", keys, err, want)
		}
	}
	must(SetStorageKeys(s, "files", "narrow", "secret"))
	must(LinkStorage(s, "web", "files"))
	st, _ = s.GetStorage(ctx(), "files")
	env := strings.Join(storageEnv(st), "\n")
	if !strings.Contains(env, "S3_ACCESS_KEY_ID=narrow") || !strings.Contains(env, "S3_BUCKET_NAME="+st.Bucket) || !strings.Contains(env, "R2_ACCOUNT_ID=acc") {
		t.Errorf("env:\n%s", env)
	}

	// Any other S3: its keys must reach its bucket from the start.
	if err := CreateStorage(s, "shop", store.Storage{Name: "media", Provider: "s3", Endpoint: r2.URL, Bucket: "media", AccessKeyID: "none", SecretAccessKey: "x"}); err == nil {
		t.Error("took keys that can't read their bucket")
	}
	if err := CreateStorage(s, "shop", store.Storage{Name: "media", Provider: "rustfs"}); err == nil {
		t.Error("made a RustFS storage")
	}
}

func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
