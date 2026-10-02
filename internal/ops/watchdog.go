package ops

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// The watchdog is a Worker in the owner's Cloudflare account that checks
// the panel every minute from outside and emails the owner when it stops
// answering: when the server, hakobu or the tunnel is down, hakobu can't
// say so itself. It mails the address notifications go to, from the same
// sender (watchdog.js).

//go:embed watchdog.js
var watchdogJS string

// watchdogCompatibilityDate is the Workers runtime version it's written for.
const watchdogCompatibilityDate = "2026-09-01"

// watchdogName names the Worker and its KV namespace after the panel, so
// two panels in one account don't share them.
func watchdogName() string {
	sum := sha256.Sum256([]byte(config.PublicHost()))
	return "hakobu-watchdog-" + hex.EncodeToString(sum[:4])
}

// WatchdogInfo describes the watchdog for Settings.
type WatchdogInfo struct {
	On        bool
	Script    string
	TurnedOff bool   // by the owner
	Err       string // why it isn't on, when hakobu tried
}

// watchdogErr is why the last attempt to deploy the watchdog failed.
var watchdogErr struct {
	sync.Mutex
	msg string
}

func setWatchdogErr(err error) {
	watchdogErr.Lock()
	defer watchdogErr.Unlock()
	watchdogErr.msg = ""
	if err != nil {
		watchdogErr.msg = err.Error()
	}
}

func Watchdog(s *store.Store) WatchdogInfo {
	off, _ := s.WatchdogTurnedOff(ctx())
	info := WatchdogInfo{TurnedOff: off}
	if w, err := s.GetWatchdog(ctx()); err == nil {
		info.On, info.Script = true, w.Script
	}
	watchdogErr.Lock()
	info.Err = watchdogErr.msg
	watchdogErr.Unlock()
	return info
}

// EnsureWatchdog deploys the watchdog where it should run and doesn't:
// emails are on, the owner didn't turn it off and the token allows. With
// redeploy it's deployed again even if it runs, for new settings.
func EnsureWatchdog(s *store.Store, redeploy bool) error {
	if _, err := s.GetNotify(ctx()); err != nil {
		return nil
	}
	if off, _ := s.WatchdogTurnedOff(ctx()); off {
		return nil
	}
	if !canRunWatchdog(TokenPermissions(s, false)) {
		setWatchdogErr(nil) // Settings shows the token's permissions
		return nil
	}
	if w, err := s.GetWatchdog(ctx()); err == nil && !redeploy {
		c, cf, err := cfClient(s)
		if err != nil {
			return err
		}
		if exists, err := c.WorkerExists(cf.AccountID, w.Script); err != nil || exists {
			return err
		}
		fmt.Println("the watchdog's Worker is gone from Cloudflare: deploying it again")
	}
	err := EnableWatchdog(s)
	setWatchdogErr(err)
	return err
}

// TurnWatchdogOn undoes the owner's TurnWatchdogOff and deploys it.
func TurnWatchdogOn(s *store.Store) error {
	if err := s.AllowWatchdog(ctx()); err != nil {
		return err
	}
	err := EnableWatchdog(s)
	setWatchdogErr(err)
	return err
}

// TurnWatchdogOff removes the watchdog and keeps hakobu from deploying it
// again.
func TurnWatchdogOff(s *store.Store) error {
	if err := s.TurnWatchdogOff(ctx()); err != nil {
		return err
	}
	setWatchdogErr(nil)
	return DisableWatchdog(s)
}

// EnableWatchdog deploys the watchdog, or deploys it again for the current
// notification settings.
func EnableWatchdog(s *store.Store) error {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return fmt.Errorf("turn notifications on first: the watchdog emails the address they go to")
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	name := watchdogName()
	kv, err := c.FindOrCreateKVNamespace(cf.AccountID, name)
	if err != nil {
		return workersHint(err)
	}
	bindings := []cloudflare.Binding{
		{"type": "send_email", "name": "EMAIL", "destination_address": n.Email},
		{"type": "kv_namespace", "name": "STATE", "namespace_id": kv},
		{"type": "plain_text", "name": "PANEL", "text": panelURL("")},
		{"type": "plain_text", "name": "FROM", "text": n.SenderName + "@" + n.SenderDomain},
		{"type": "plain_text", "name": "TO", "text": n.Email},
	}
	if err := c.UploadWorker(cf.AccountID, name, watchdogJS, watchdogCompatibilityDate, bindings); err != nil {
		return workersHint(err)
	}
	if err := c.SetWorkerCrons(cf.AccountID, name, []string{"* * * * *"}); err != nil {
		return workersHint(err)
	}
	return s.SaveWatchdog(ctx(), store.SaveWatchdogParams{Script: name, KvNamespaceID: kv})
}

// DisableWatchdog removes the watchdog's Worker and KV namespace.
func DisableWatchdog(s *store.Store) error {
	w, err := s.GetWatchdog(ctx())
	if err != nil {
		return nil
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	if err := c.DeleteWorker(cf.AccountID, w.Script); err != nil {
		return workersHint(err)
	}
	if err := c.DeleteKVNamespace(cf.AccountID, w.KvNamespaceID); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
		return workersHint(err)
	}
	return s.DeleteWatchdog(ctx())
}

// workersHint names the permissions the watchdog needs when Cloudflare
// refused a valid token: one made before the watchdog lacks them.
func workersHint(err error) error {
	if err == nil || !strings.Contains(err.Error(), cloudflare.TokenLacksPermission) {
		return err
	}
	return fmt.Errorf("%w; the watchdog needs Workers Scripts Edit and Workers KV Storage Edit: add them to hakobu's token in Cloudflare (Manage account → API Tokens → Edit), or give hakobu a new token with `cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect`", err)
}
