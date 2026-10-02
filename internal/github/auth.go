// Package github is a minimal client for the GitHub App hakobu registers for
// itself: installation tokens for cloning, the manifest flow for setup and
// OAuth for signing in.
package github

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// httpClient gives up on GitHub after a while: a hung request would
// otherwise hold a deploy, and with it the app, forever.
var httpClient = &http.Client{Timeout: time.Minute}

func appJWT(appID int64, privateKeyPEM string) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", appID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(key)
}

// call performs a GitHub API request and decodes a JSON response with the
// expected status into out.
func call(method, rawURL, bearer string, body io.Reader, wantStatus int, out any) error {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("github api %s %s: %d: %s", method, rawURL, resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func InstallationToken(appID int64, privateKeyPEM string, installationID int64) (string, error) {
	j, err := appJWT(appID, privateKeyPEM)
	if err != nil {
		return "", err
	}
	var res struct {
		Token string `json:"token"`
	}
	err = call("POST", fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID), j, nil, http.StatusCreated, &res)
	return res.Token, err
}

// RepoToken returns an installation token that can read repo ("owner/name").
func RepoToken(appID int64, privateKeyPEM, repo string) (string, error) {
	j, err := appJWT(appID, privateKeyPEM)
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := call("GET", "https://api.github.com/repos/"+repo+"/installation", j, nil, http.StatusOK, &inst); err != nil {
		return "", fmt.Errorf("repo %s is not accessible — install the GitHub App on it: %w", repo, err)
	}
	return InstallationToken(appID, privateKeyPEM, inst.ID)
}

// SetWebhookSecret changes the secret GitHub signs the app's webhook
// deliveries with.
func SetWebhookSecret(appID int64, privateKeyPEM, secret string) error {
	j, err := appJWT(appID, privateKeyPEM)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"secret": secret})
	return call("PATCH", "https://api.github.com/app/hook/config", j, bytes.NewReader(body), http.StatusOK, nil)
}

func CloneURL(repo string) string {
	return "https://github.com/" + repo + ".git"
}

// ListRepos returns every repo reachable through any installation of the app.
func ListRepos(appID int64, privateKeyPEM string) ([]string, error) {
	j, err := appJWT(appID, privateKeyPEM)
	if err != nil {
		return nil, err
	}
	var installations []struct {
		ID int64 `json:"id"`
	}
	if err := call("GET", "https://api.github.com/app/installations?per_page=100", j, nil, http.StatusOK, &installations); err != nil {
		return nil, err
	}
	var repos []string
	for _, inst := range installations {
		token, err := InstallationToken(appID, privateKeyPEM, inst.ID)
		if err != nil {
			return nil, err
		}
		for page := 1; page <= 10; page++ {
			var res struct {
				Repositories []struct {
					FullName string `json:"full_name"`
				} `json:"repositories"`
			}
			if err := call("GET", fmt.Sprintf("https://api.github.com/installation/repositories?per_page=100&page=%d", page), token, nil, http.StatusOK, &res); err != nil {
				return nil, err
			}
			for _, r := range res.Repositories {
				repos = append(repos, r.FullName)
			}
			if len(res.Repositories) < 100 {
				break
			}
		}
	}
	return repos, nil
}

// RepoFiles lists every file path on the repo's default branch. GitHub
// truncates very large trees, which only affects deep paths we don't scan.
func RepoFiles(repo, token string) ([]string, error) {
	var res struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := call("GET", "https://api.github.com/repos/"+repo+"/git/trees/HEAD?recursive=1", token, nil, http.StatusOK, &res); err != nil {
		return nil, err
	}
	var files []string
	for _, e := range res.Tree {
		if e.Type == "blob" {
			files = append(files, e.Path)
		}
	}
	return files, nil
}

// FileContent reads a file from the repo's default branch; ok is false if it
// doesn't exist.
func FileContent(repo, path, token string) (content string, ok bool) {
	var res struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if call("GET", "https://api.github.com/repos/"+repo+"/contents/"+path, token, nil, http.StatusOK, &res) != nil || res.Encoding != "base64" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(res.Content, "\n", ""))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// BuildManifest describes the GitHub App for the manifest flow
// (https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):
// read-only contents, push events, webhook and OAuth pointing at publicHost.
func BuildManifest(publicHost string) ([]byte, error) {
	base := "https://" + publicHost
	// App names are globally unique on GitHub and capped at 34 characters;
	// the random suffix keeps reinstalls on the same host from colliding.
	host := strings.ReplaceAll(strings.ToLower(publicHost), ".", "-")
	if len(host) > 22 {
		host = strings.TrimRight(host[:22], "-")
	}
	suffix := make([]byte, 2)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("hakobu-%s-%x", host, suffix)
	return json.Marshal(map[string]any{
		"name":                name,
		"url":                 base,
		"hook_attributes":     map[string]string{"url": base + "/webhook/github"},
		"redirect_url":        base + "/github-app/callback",
		"callback_urls":       []string{base + "/auth/callback"},
		"public":              false,
		"default_permissions": map[string]string{"contents": "read", "metadata": "read", "email_addresses": "read"},
		"default_events":      []string{"push"},
	})
}

type ManifestConversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
}

func ConvertManifestCode(code string) (*ManifestConversion, error) {
	var mc ManifestConversion
	err := call("POST", "https://api.github.com/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, http.StatusCreated, &mc)
	if err != nil {
		return nil, err
	}
	return &mc, nil
}

// AuthorizeURL starts a sign-in; with pickAccount GitHub asks which account
// to use even if one is signed in, so the person has to act.
func AuthorizeURL(clientID, redirectURI, state string, pickAccount bool) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("state", state)
	if pickAccount {
		v.Set("prompt", "select_account")
	}
	return "https://github.com/login/oauth/authorize?" + v.Encode()
}

// User is a GitHub account. ID never changes; Login can be renamed, and a
// freed login can be taken by someone else.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"` // primary and verified, or the public one; may be ""
}

// SignIn exchanges an OAuth callback code for the signed-in user.
func SignIn(clientID, clientSecret, code, redirectURI string) (User, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequest("POST", "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return User{}, err
	}
	if tok.AccessToken == "" {
		return User{}, fmt.Errorf("github oauth error: %s (%s)", tok.Error, tok.ErrorDescription)
	}

	var user User
	if err := call("GET", "https://api.github.com/user", tok.AccessToken, nil, http.StatusOK, &user); err != nil {
		return User{}, err
	}
	if user.ID == 0 {
		return User{}, fmt.Errorf("github returned no user ID")
	}
	// The primary address, if the App may read the user's addresses (Apps
	// registered before it asked can't); otherwise the public one stays.
	var emails []struct {
		Email             string `json:"email"`
		Primary, Verified bool
	}
	if call("GET", "https://api.github.com/user/emails", tok.AccessToken, nil, http.StatusOK, &emails) == nil {
		for _, e := range emails {
			if e.Primary && e.Verified {
				user.Email = e.Email
			}
		}
	}
	return user, nil
}
