package cloudflare

import (
	"strings"
	"sync"
)

// A token can't be asked for its permissions without one more (API
// Tokens Read), so hakobu asks for something each permission lets it read
// and sees what Cloudflare refuses. Reading can't tell Read from Edit, but
// tokens from the template have Edit throughout.

// Permission is one of hakobu's token permissions, what it's for, and
// whether the token has it.
type Permission struct {
	Name    string // as the dashboard shows it
	For     string // what hakobu does with it
	Missing bool   // Cloudflare refused the token
	Unknown bool   // the check failed for another reason
}

// Lacking lists the permissions the token is missing.
func Lacking(perms []Permission) []Permission {
	var out []Permission
	for _, p := range perms {
		if p.Missing {
			out = append(out, p)
		}
	}
	return out
}

// CheckPermissions reads with the token what each of hakobu's permissions
// allows, in the account and, for the zone permissions, in zoneID.
func (c Client) CheckPermissions(accountID, zoneID string) []Permission {
	c.AccountID = accountID
	checks := []struct {
		perm Permission
		path string
	}{
		{Permission{Name: "Zone Read", For: "listing your domains"}, "/zones?per_page=1"},
		{Permission{Name: "DNS Edit", For: "addresses of the panel and apps"}, "/zones/" + zoneID + "/dns_records?per_page=1"},
		{Permission{Name: "Cloudflare Tunnel Edit", For: "the tunnel to this server"}, "/accounts/" + accountID + "/cfd_tunnel?per_page=1"},
		{Permission{Name: "Workers R2 Storage Edit", For: "backups"}, "/accounts/" + accountID + "/r2/buckets?per_page=1"},
		{Permission{Name: "Zone Settings Edit", For: "Email Routing for the emails' sender"}, "/zones/" + zoneID + "/email/routing"},
		{Permission{Name: "Email Routing Addresses Edit", For: "the address emails go to"}, "/accounts/" + accountID + "/email/routing/addresses?per_page=1"},
		{Permission{Name: "Workers Scripts Edit", For: "the watchdog"}, "/accounts/" + accountID + "/workers/scripts"},
		{Permission{Name: "Workers KV Storage Edit", For: "the watchdog"}, "/accounts/" + accountID + "/storage/kv/namespaces?per_page=1"},
		{Permission{Name: "D1 Edit", For: "uptime history"}, "/accounts/" + accountID + "/d1/database?per_page=1"},
	}
	out := make([]Permission, len(checks))
	var wg sync.WaitGroup
	for i, ch := range checks {
		out[i] = ch.perm
		if strings.Contains(ch.path, "/zones//") { // no panel domain yet
			out[i].Unknown = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.call("GET", ch.path, nil, nil); err != nil {
				if strings.Contains(err.Error(), TokenLacksPermission) {
					out[i].Missing = true
				} else {
					out[i].Unknown = true
				}
			}
		}()
	}
	wg.Wait()
	return out
}
