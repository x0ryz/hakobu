package cloudflare

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Email to the owner goes through Cloudflare Email Service. Sending to a
// verified destination address (one whose owner clicked the link in
// Cloudflare's confirmation email) is free on every plan, but the sender
// must be on a domain with Email Routing on.

// Routing is a zone's Email Routing.
type Routing struct {
	// ApexReady: on and working for the zone's apex, whose MX records
	// point at Cloudflare. Turning it on for a subdomain marks the zone
	// enabled too, so that alone means nothing.
	ApexReady  bool
	Subdomains []string // where it's on and working
}

// EmailRouting tells where the zone has Email Routing working.
func (c Client) EmailRouting(zoneID string) (Routing, error) {
	var r struct {
		Enabled    bool   `json:"enabled"`
		Status     string `json:"status"`
		Subdomains []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Status  string `json:"status"`
		} `json:"subdomains"`
	}
	if err := c.call("GET", "/zones/"+zoneID+"/email/routing", nil, &r); err != nil {
		return Routing{}, err
	}
	out := Routing{ApexReady: r.Enabled && r.Status == "ready"}
	for _, s := range r.Subdomains {
		if s.Enabled && s.Status == "ready" {
			out.Subdomains = append(out.Subdomains, s.Name)
		}
	}
	return out, nil
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

// DisableEmailRouting turns Email Routing off for name and removes its
// records; never for a whole zone by accident, name is required.
func (c Client) DisableEmailRouting(zoneID, name string) error {
	if name == "" {
		return errors.New("disabling Email Routing needs the domain")
	}
	return c.call("DELETE", "/zones/"+zoneID+"/email/routing/dns", map[string]string{"name": name}, nil)
}

// MXRecords returns the mail servers of name.
func (c Client) MXRecords(zoneID, name string) ([]string, error) {
	var recs []struct {
		Content string `json:"content"`
	}
	q := url.Values{"type": {"MX"}, "name": {name}}
	if err := c.call("GET", "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.Content)
	}
	return out, nil
}

// CloudflareSPF is the SPF record Email Routing writes.
const CloudflareSPF = `"v=spf1 include:_spf.mx.cloudflare.net ~all"`

// SPFRecords returns the IDs of the SPF records of name, by content.
func (c Client) SPFRecords(zoneID, name string) (map[string]string, error) {
	var recs []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	q := url.Values{"type": {"TXT"}, "name": {name}}
	if err := c.call("GET", "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range recs {
		if strings.Contains(r.Content, "v=spf1") {
			out[r.ID] = r.Content
		}
	}
	return out, nil
}

// SetTXT sets the content of a TXT record.
func (c Client) SetTXT(zoneID, recordID, content string) error {
	return c.call("PATCH", "/zones/"+zoneID+"/dns_records/"+recordID, map[string]string{"content": content}, nil)
}

// Destination is a verified, or still to verify, address of the account.
type Destination struct {
	ID       string
	Email    string
	Verified bool
}

// Destinations lists the account's destination addresses.
func (c Client) Destinations(accountID string) ([]Destination, error) {
	var out []Destination
	for page := 1; ; page++ {
		var list []struct {
			ID       string  `json:"id"`
			Email    string  `json:"email"`
			Verified *string `json:"verified"`
		}
		q := url.Values{"page": {fmt.Sprint(page)}, "per_page": {"50"}}
		if err := c.call("GET", "/accounts/"+accountID+"/email/routing/addresses?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list {
			out = append(out, Destination{ID: a.ID, Email: a.Email, Verified: a.Verified != nil && *a.Verified != ""})
		}
		if len(list) < 50 {
			return out, nil
		}
	}
}

// errNoDestination: the address isn't one of the account's.
var errNoDestination = errors.New("not a destination address of the account")

// Destination looks email up among the account's destination addresses.
func (c Client) Destination(accountID, email string) (Destination, error) {
	list, err := c.Destinations(accountID)
	if err != nil {
		return Destination{}, err
	}
	for _, d := range list {
		if strings.EqualFold(d.Email, email) {
			return d, nil
		}
	}
	return Destination{}, errNoDestination
}

// AddDestination adds email to the account's destination addresses;
// Cloudflare mails it a link to confirm. added is false if it was there
// already.
func (c Client) AddDestination(accountID, email string) (added bool, err error) {
	if _, err := c.Destination(accountID, email); err == nil {
		return false, nil
	} else if !errors.Is(err, errNoDestination) {
		return false, err
	}
	return true, c.call("POST", "/accounts/"+accountID+"/email/routing/addresses", map[string]string{"email": email}, nil)
}

// DeleteDestination removes email from the account's destination
// addresses; one that isn't there is no error.
func (c Client) DeleteDestination(accountID, email string) error {
	d, err := c.Destination(accountID, email)
	if errors.Is(err, errNoDestination) {
		return nil
	} else if err != nil {
		return err
	}
	return c.call("DELETE", "/accounts/"+accountID+"/email/routing/addresses/"+d.ID, nil, nil)
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
