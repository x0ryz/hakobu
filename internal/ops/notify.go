package ops

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// The owner gets an email when something needs them: a deploy after a push
// failed, a backup failed, an app ran out of memory, the disk is filling
// up. It goes through Cloudflare Email Service to an address the owner
// confirmed, which is free, from hakobu@ a domain with Email Routing on:
// the zone's apex if it already has it, otherwise mail.<panel host>, so
// the mail of the owner's own domain is never touched.
//
// A problem is mailed once, then again only if it's still there hours
// later; when it's gone, one more email says so. What was mailed is kept
// in memory, so a restart may repeat an email.

// notifyAgain is how long a problem that's still there stays quiet.
const notifyAgain = 6 * time.Hour

var problems struct {
	sync.Mutex
	mailed map[string]time.Time // by problem key, when last mailed
}

// errNotifyOff: nobody to mail.
var errNotifyOff = errors.New("notifications are off")

// NotifyInfo describes the notifications for Settings.
type NotifyInfo struct {
	On       bool
	Email    string
	From     string
	Verified bool   // the owner confirmed the address
	Err      string // why it couldn't be checked
}

// confirmed remembers addresses known to be confirmed, so Settings asks
// Cloudflare only until the owner clicks the link.
var confirmed sync.Map

// Notifications reports where emails go and whether the address is
// confirmed.
func Notifications(s *store.Store) NotifyInfo {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return NotifyInfo{}
	}
	info := NotifyInfo{On: true, Email: n.Email, From: "hakobu@" + n.SenderDomain}
	if _, ok := confirmed.Load(n.Email); ok {
		info.Verified = true
		return info
	}
	c, cf, err := cfClient(s)
	if err == nil {
		_, info.Verified, err = c.Destination(cf.AccountID, n.Email)
	}
	if err != nil {
		info.Err = tokenHint(err).Error()
	} else if info.Verified {
		confirmed.Store(n.Email, true)
	}
	return info
}

// SetupNotifications sends emails to email from now on, from the domain
// from if given (one of the account's), and asks Cloudflare to have the
// address confirmed.
func SetupNotifications(s *store.Store, email, from string) error {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || addr.Name != "" {
		return fmt.Errorf("%q isn't an email address", email)
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	if cf.AccountID == "" {
		return fmt.Errorf("finish `hakobu setup` first")
	}
	zones, err := c.Zones()
	if err != nil {
		return err
	}
	sender, err := senderDomain(c, zones, strings.Trim(strings.ToLower(strings.TrimSpace(from)), "."))
	if err != nil {
		return tokenHint(err)
	}
	if err := c.AddDestination(cf.AccountID, addr.Address); err != nil {
		return tokenHint(err)
	}
	return s.SaveNotify(ctx(), store.SaveNotifyParams{Email: addr.Address, SenderDomain: sender})
}

// senderDomain picks the domain to send from, turning Email Routing on for
// it if needed. It never turns it on for a zone's apex: that replaces the
// domain's MX records, and with them its mail.
func senderDomain(c cloudflare.Client, zones []cloudflare.Zone, from string) (string, error) {
	host := from
	if host == "" {
		host = config.PublicHost()
	}
	zone, ok := cloudflare.ZoneFor(zones, host)
	if !ok {
		return "", fmt.Errorf("%s is not in a domain of your Cloudflare account", host)
	}
	apexOn, err := c.EmailRouting(zone.ID)
	if err != nil {
		return "", err
	}
	switch {
	case from == zone.Name && !apexOn:
		return "", fmt.Errorf("Email Routing is off for %s; turning it on would replace the domain's MX records, so hakobu doesn't. Turn it on in Cloudflare, or send from a subdomain", from)
	case from == zone.Name || (from == "" && apexOn):
		return zone.Name, nil
	case from == "":
		from = "mail." + host
	}
	return from, c.EnableEmailRouting(zone.ID, from)
}

// tokenHint explains a refusal by Cloudflare that's down to a token made
// before hakobu sent email.
func tokenHint(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "authentication") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "permission") {
		return fmt.Errorf("%w; the Cloudflare token needs Zone Settings Edit, Email Routing Addresses Edit and Email Sending Edit: give hakobu a new one with `cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect`", err)
	}
	return err
}

// TurnOffNotifications stops the emails. The address stays confirmed in
// Cloudflare, and Email Routing on, for turning them on again.
func TurnOffNotifications(s *store.Store) error {
	return s.DeleteNotify(ctx())
}

// SendTestEmail sends an email right away.
func SendTestEmail(s *store.Store) error {
	return mailOwner(s, "Test email", "Notifications from your hakobu panel arrive here.")
}

// mailOwner sends an email to the owner, if notifications are on.
func mailOwner(s *store.Store, subject, text string) error {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return errNotifyOff
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	host := config.PublicHost()
	err = c.SendEmail(cf.AccountID, cloudflare.Email{
		FromAddress: "hakobu@" + n.SenderDomain,
		FromName:    "Hakobu",
		To:          n.Email,
		Subject:     subject,
		Text:        text + "\n\n-- \nhakobu at https://" + host + "\nTurn these emails off in Settings: https://" + host + "/settings#notifications\n",
	})
	return tokenHint(err)
}

// problem mails the owner about the problem key unless it was mailed
// within again.
func problem(s *store.Store, key string, again time.Duration, subject, text string) {
	if _, err := s.GetNotify(ctx()); err != nil {
		return
	}
	problems.Lock()
	if problems.mailed == nil {
		problems.mailed = map[string]time.Time{}
	}
	last, ok := problems.mailed[key]
	if ok && time.Since(last) < again {
		problems.Unlock()
		return
	}
	problems.mailed[key] = time.Now()
	problems.Unlock()
	async(func() { sendOrLog(s, subject, text) })
}

// solved mails the owner that the problem key is gone, if it was mailed;
// with subject "" it only forgets it.
func solved(s *store.Store, key, subject, text string) {
	problems.Lock()
	_, ok := problems.mailed[key]
	delete(problems.mailed, key)
	problems.Unlock()
	if ok && subject != "" {
		async(func() { sendOrLog(s, subject, text) })
	}
}

// async runs an email's sending off the caller's path; tests wait for it.
var async = func(f func()) { go f() }

func sendOrLog(s *store.Store, subject, text string) {
	if err := mailOwner(s, subject, text); err != nil && !errors.Is(err, errNotifyOff) {
		fmt.Println("failed to email the owner:", err)
	}
}

func panelURL(path string) string { return "https://" + config.PublicHost() + path }

// noteDeploy mails a deploy that failed after a push, when nobody was
// watching, and the next one that works.
func noteDeploy(s *store.Store, app, trigger string, err error) {
	key := "deploy:" + app
	if err == nil {
		solved(s, key, app+": deploys work again", "The latest deploy of "+app+" succeeded.\n\n"+panelURL("/apps/"+app))
		return
	}
	if trigger != "push" {
		return
	}
	problem(s, key, notifyAgain, app+": deploy failed",
		fmt.Sprintf("Deploying %s after a push to GitHub failed:\n\n  %v\n\nThe deploy log: %s", app, err, panelURL("/apps/"+app+"#deployments")))
}

// NoteBackup mails a failed backup of a database, or of the panel with
// name "panel", and the next one that works.
func NoteBackup(s *store.Store, name string, err error) {
	what, path := "database "+name, "/databases/"+name
	if name == "panel" {
		what, path = "the panel", "/settings"
	}
	key := "backup:" + name
	if err == nil {
		solved(s, key, "Backups of "+what+" work again", "The latest backup of "+what+" succeeded.\n\n"+panelURL(path))
		return
	}
	problem(s, key, notifyAgain, "Backup of "+what+" failed",
		fmt.Sprintf("Backing up %s failed:\n\n  %v\n\nhakobu tries again within the hour. %s", what, err, panelURL(path)))
}

// noteOOM mails an app or worker killed for running out of memory.
func noteOOM(s *store.Store, app, message string) {
	problem(s, "oom:"+app, notifyAgain, app+": out of memory",
		message+"\n\nGive it more memory or find what grows: "+panelURL("/apps/"+app+"#errors"))
}

// CheckForOwner mails the disk filling up and a master key the owner
// hasn't downloaded; it's called hourly.
func CheckForOwner(s *store.Store) {
	if disk, low := DiskUsage(); low {
		problem(s, "disk", notifyAgain, "The server's disk is almost full",
			"Disk: "+disk+". Deploys and databases fail on a full disk; free some space or clean up: "+panelURL("/settings"))
	} else {
		solved(s, "disk", "The server's disk has room again", "Disk: "+disk+".")
	}
	if BackupBucket(s) != "" && panelBackedUp() && !KeyDownloaded() {
		problem(s, "key", 7*24*time.Hour, "Download the panel's master key",
			"The panel is backed up, but you have no copy of its current master key, and without it the backups can't be restored. Download it and keep it away from the server: "+panelURL("/settings/master-key"))
	} else {
		solved(s, "key", "", "")
	}
}
