package ops

import (
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

// tokenChecked is how long a check of the token's permissions holds.
const tokenChecked = 10 * time.Minute

var tokenPerms struct {
	sync.Mutex
	at    time.Time
	perms []cloudflare.Permission
}

// TokenPermissions checks what hakobu's Cloudflare token can do, at most
// every ten minutes unless fresh.
func TokenPermissions(s *store.Store, fresh bool) []cloudflare.Permission {
	tokenPerms.Lock()
	if !fresh && time.Since(tokenPerms.at) < tokenChecked {
		defer tokenPerms.Unlock()
		return tokenPerms.perms
	}
	tokenPerms.Unlock()
	c, cf, err := cfClient(s)
	if err != nil || cf.AccountID == "" {
		return nil
	}
	perms := c.CheckPermissions(cf.AccountID, cf.PanelZoneID)
	tokenPerms.Lock()
	tokenPerms.at, tokenPerms.perms = time.Now(), perms
	tokenPerms.Unlock()
	return perms
}

// CachedTokenPermissions is the last check, without asking Cloudflare.
func CachedTokenPermissions() []cloudflare.Permission {
	tokenPerms.Lock()
	defer tokenPerms.Unlock()
	return tokenPerms.perms
}

// canRunWatchdog: the token isn't known to lack what the watchdog needs.
func canRunWatchdog(perms []cloudflare.Permission) bool {
	for _, p := range cloudflare.Lacking(perms) {
		if p.For == "the watchdog" {
			return false
		}
	}
	return true
}

// ReplaceCloudflareToken takes a new token for the same account, then sets
// up again what the old one couldn't.
func ReplaceCloudflareToken(s *store.Store, token string) error {
	if _, err := ConnectCloudflare(s, token); err != nil {
		return err
	}
	TokenPermissions(s, true)
	return EnsureWatchdog(s, false)
}
