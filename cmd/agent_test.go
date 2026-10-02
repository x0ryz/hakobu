package cmd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestWebhookRefusesReplays(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGitHubApp(t.Context(), store.SaveGitHubAppParams{AppID: 1, WebhookSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	h := webhookHandler(s)
	send := func(delivery string, pushedAt time.Time, sign string) int {
		body := fmt.Sprintf(`{"ref":"refs/heads/main","repository":{"full_name":"me/web","default_branch":"main","pushed_at":%d}}`, pushedAt.Unix())
		mac := hmac.New(sha256.New, []byte(sign))
		mac.Write([]byte(body))
		r := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-GitHub-Delivery", delivery)
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	now := time.Now()
	for _, c := range []struct {
		name, delivery string
		pushedAt       time.Time
		sign           string
		want           int
	}{
		{"a push", "d1", now, "secret", http.StatusOK},
		{"the same delivery again", "d1", now, "secret", http.StatusConflict},
		{"another delivery", "d2", now, "secret", http.StatusOK},
		{"a push from two days ago", "d3", now.Add(-48 * time.Hour), "secret", http.StatusConflict},
		{"a wrong signature", "d4", now, "guess", http.StatusUnauthorized},
		{"no delivery ID", "", now, "secret", http.StatusBadRequest},
	} {
		if got := send(c.delivery, c.pushedAt, c.sign); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
