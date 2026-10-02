package cloudflare

import (
	"fmt"
	"net/url"
	"strings"
)

// Email to the owner goes through Cloudflare Email Service. Sending to a
// verified destination address (one whose owner clicked the link in
// Cloudflare's confirmation email) is free on every plan, but the sender
// must be on a domain with Email Routing on.

// EmailRouting reports whether Email Routing is on for the zone's apex.
func (c Client) EmailRouting(zoneID string) (enabled bool, err error) {
	var r struct {
		Enabled bool `json:"enabled"`
	}
	err = c.call("GET", "/zones/"+zoneID+"/email/routing", nil, &r)
	return r.Enabled, err
}

// EnableEmailRouting turns Email Routing on for name, the zone's apex or
// one of its subdomains: Cloudflare adds its MX, SPF and DKIM records
// there. Turning it on again is no error.
func (c Client) EnableEmailRouting(zoneID, name string) error {
	err := c.call("POST", "/zones/"+zoneID+"/email/routing/dns", map[string]string{"name": name}, nil)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already") {
		return nil
	}
	return err
}

// AddDestination adds email to the account's destination addresses;
// Cloudflare mails it a link to confirm. Adding it again is no error.
func (c Client) AddDestination(accountID, email string) error {
	if _, _, err := c.Destination(accountID, email); err == nil {
		return nil
	}
	return c.call("POST", "/accounts/"+accountID+"/email/routing/addresses", map[string]string{"email": email}, nil)
}

// errNoDestination: the address isn't one of the account's.
var errNoDestination = fmt.Errorf("not a destination address of the account")

// Destination looks email up among the account's destination addresses;
// verified tells whether its owner confirmed it.
func (c Client) Destination(accountID, email string) (id string, verified bool, err error) {
	for page := 1; ; page++ {
		var list []struct {
			ID       string  `json:"id"`
			Email    string  `json:"email"`
			Verified *string `json:"verified"`
		}
		q := url.Values{"page": {fmt.Sprint(page)}, "per_page": {"50"}}
		if err := c.call("GET", "/accounts/"+accountID+"/email/routing/addresses?"+q.Encode(), nil, &list); err != nil {
			return "", false, err
		}
		for _, a := range list {
			if strings.EqualFold(a.Email, email) {
				return a.ID, a.Verified != nil && *a.Verified != "", nil
			}
		}
		if len(list) < 50 {
			return "", false, errNoDestination
		}
	}
}

// Email is a plain-text message.
type Email struct {
	FromAddress, FromName string
	To                    string
	Subject, Text         string
}

// SendEmail sends m through Email Service.
func (c Client) SendEmail(accountID string, m Email) error {
	return c.call("POST", "/accounts/"+accountID+"/email/sending/send", map[string]any{
		"from":    map[string]string{"address": m.FromAddress, "name": m.FromName},
		"to":      m.To,
		"subject": m.Subject,
		"text":    m.Text,
	}, nil)
}
