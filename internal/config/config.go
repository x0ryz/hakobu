package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	AgentAddr      = envString("HAKOBU_ADDR", "127.0.0.1:9000")
	ReadTimeout    = envDuration("HAKOBU_READ_TIMEOUT", 15*time.Second)
	WriteTimeout   = envDuration("HAKOBU_WRITE_TIMEOUT", 5*time.Minute)
	IdleTimeout    = envDuration("HAKOBU_IDLE_TIMEOUT", 120*time.Second)
	ProxyPollEvery = envDuration("HAKOBU_PROXY_POLL", 10*time.Second)
	BackupEvery    = envDuration("HAKOBU_BACKUP_EVERY", 24*time.Hour)
	BackupKeep     = envInt("HAKOBU_BACKUP_KEEP", 7) // plus one a week for four weeks
	RetentionDays  = envInt("HAKOBU_RETENTION_DAYS", 7)
)

const (
	publicHostFile = "data/public_host"
	appsDomainFile = "data/apps_domain"
	setupTokenFile = "data/setup_token"
)

// PrepareDataDir makes data/ (database, secrets, clones) private to the user
// hakobu runs as, including on installs that created it world-readable, and
// makes every file hakobu creates from now on private too.
func PrepareDataDir() error {
	syscall.Umask(0o077)
	if err := os.MkdirAll("data", 0o700); err != nil {
		return err
	}
	return os.Chmod("data", 0o700)
}

// PublicHost is the panel's hostname (no scheme), chosen by `hakobu setup`.
// HAKOBU_PUBLIC_HOST overrides it.
func PublicHost() string {
	if v := os.Getenv("HAKOBU_PUBLIC_HOST"); v != "" {
		return v
	}
	return readFile(publicHostFile)
}

func SetPublicHost(host string) error {
	return writeFile(publicHostFile, strings.TrimSpace(host))
}

// AppsDomain is the zone the panel lives in; new apps default to
// <app>.<AppsDomain>.
func AppsDomain() string {
	return readFile(appsDomainFile)
}

func SetAppsDomain(zone string) error {
	return writeFile(appsDomainFile, zone)
}

// SetupToken gates first-run setup until an owner has signed in.
func SetupToken() string {
	return readFile(setupTokenFile)
}

func SetSetupToken(token string) error {
	return writeFile(setupTokenFile, token)
}

func ClearSetupToken() error {
	err := os.Remove(setupTokenFile)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value+"\n"), 0o600)
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}
