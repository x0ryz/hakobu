package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// The tunnel runs as its own container, so restarting or upgrading hakobu
// doesn't take the apps down: cloudflared sends each app's domain straight
// to the app's live container on the edge network, and everything else to
// the panel's unix socket.
const (
	// PanelSocketDir holds panel.sock, the panel's socket for cloudflared.
	PanelSocketDir = "run"
	panelService   = "unix:/run/hakobu/panel.sock"
)

// tunnelContainer runs cloudflared; tests use another name.
var tunnelContainer = "hakobu-cloudflared"

// EdgeAlias is the app's name on the edge network. App and container names
// have no dots, so it can't clash with a container name.
func EdgeAlias(app string) string { return app + ".hakobu" }

// StartTunnel runs cloudflared once `hakobu setup` has created the tunnel,
// leaving an already running one alone.
func StartTunnel(s *store.Store) error {
	cf, err := s.GetCloudflare(ctx())
	if err != nil || cf.TunnelToken == "" {
		return nil
	}
	env, err := deploy.ContainerEnv(ctx(), tunnelContainer)
	if err != nil {
		return err
	}
	if env != nil && env["TUNNEL_TOKEN"] == string(cf.TunnelToken) {
		err = deploy.StartContainer(ctx(), tunnelContainer)
	} else {
		var dir string
		if dir, err = filepath.Abs(PanelSocketDir); err == nil {
			_, err = deploy.RunTunnelContainer(ctx(), tunnelContainer, config.CloudflaredImage, string(cf.TunnelToken), dir)
		}
	}
	if err != nil {
		return err
	}
	// A new container is on no project's edge network yet.
	return ensureAllProjectNetworks(s)
}

var (
	ingressMu sync.Mutex
	applied   string // the ingress last sent to Cloudflare
)

// SyncTunnel points every public app's domain at its live container, if
// that changed since the last sync.
func SyncTunnel(s *store.Store) error {
	ingressMu.Lock()
	defer ingressMu.Unlock()
	if !TunnelReady(s) {
		return nil
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return err
	}
	var rules []cloudflare.IngressRule
	for _, a := range apps {
		if a.Domain != "" && a.LivePort > 0 {
			rules = append(rules, cloudflare.IngressRule{Hostname: a.Domain, Service: fmt.Sprintf("http://%s:%d", EdgeAlias(a.Name), a.LivePort)})
		}
	}
	rules = append(rules, cloudflare.IngressRule{Service: panelService})
	key := fmt.Sprint(rules)
	if key == applied {
		return nil
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	if err := c.SetIngress(cf.AccountID, cf.TunnelID, rules); err != nil {
		return err
	}
	applied = key
	return nil
}

func CloudflareConnected(s *store.Store) bool {
	_, err := s.GetCloudflare(ctx())
	return err == nil
}

func TunnelReady(s *store.Store) bool {
	cf, err := s.GetCloudflare(ctx())
	return err == nil && cf.TunnelID != ""
}

// ConnectCloudflare checks token and saves it, returning the domains it
// sees; the tunnel and the panel's record keep working with a new token of
// the same account.
func ConnectCloudflare(s *store.Store, token string) ([]cloudflare.Zone, error) {
	token = strings.TrimSpace(token)
	zones, err := cloudflare.Client{Token: token}.CheckToken()
	if err != nil {
		return nil, err
	}
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.AccountID != "" {
		same := false
		for _, z := range zones {
			same = same || z.Account.ID == cf.AccountID
		}
		if !same {
			return nil, fmt.Errorf("the token is for another Cloudflare account than the one hakobu's tunnel is in")
		}
	}
	return zones, s.SaveCloudflareToken(ctx(), secret.String(token))
}

// cfClient returns an API client with the saved token.
func cfClient(s *store.Store) (cloudflare.Client, store.Cloudflare, error) {
	cf, err := s.GetCloudflare(ctx())
	if err != nil {
		return cloudflare.Client{}, cf, fmt.Errorf("Cloudflare is not connected")
	}
	return cloudflare.Client{Token: string(cf.ApiToken)}, cf, nil
}

func Zones(s *store.Store) ([]cloudflare.Zone, error) {
	c, _, err := cfClient(s)
	if err != nil {
		return nil, err
	}
	return c.Zones()
}

// SetupTunnel creates the tunnel and points <sub>.<zone> at it for the
// panel. It returns the panel's host.
func SetupTunnel(s *store.Store, zoneID, sub string) (string, error) {
	c, _, err := cfClient(s)
	if err != nil {
		return "", err
	}
	zones, err := c.Zones()
	if err != nil {
		return "", err
	}
	var zone cloudflare.Zone
	for _, z := range zones {
		if z.ID == zoneID {
			zone = z
		}
	}
	if zone.ID == "" {
		return "", fmt.Errorf("domain not found in the connected Cloudflare account")
	}
	sub = strings.Trim(strings.ToLower(strings.TrimSpace(sub)), ".")
	host := zone.Name
	if sub != "" {
		host = sub + "." + zone.Name
	}

	hostname, _ := os.Hostname()
	suffix, _ := RandomHex(3)
	tunnelID, token, err := c.CreateTunnel(zone.Account.ID, "hakobu-"+strings.Split(hostname, ".")[0]+"-"+suffix, panelService)
	if err != nil {
		return "", err
	}
	recordID, err := c.RouteHost(zone.ID, host, tunnelID)
	if err != nil {
		return "", err
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{
		AccountID: zone.Account.ID, TunnelID: tunnelID, TunnelToken: secret.String(token), PanelZoneID: zone.ID, PanelRecordID: recordID,
	}); err != nil {
		return "", err
	}
	if err := config.SetPublicHost(host); err != nil {
		return "", err
	}
	if err := config.SetAppsDomain(zone.Name); err != nil {
		return "", err
	}
	return host, nil
}

// SetAppDomain points domain at the tunnel (replacing the app's previous
// record) or, with domain "", makes the app private.
func SetAppDomain(s *store.Store, app store.App, domain string) error {
	domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == app.Domain {
		return nil
	}
	if err := CheckDomain(s, app.Name, domain); err != nil {
		return err
	}
	params := store.SetAppDomainParams{Name: app.Name, Domain: domain}
	connected := TunnelReady(s)
	var c cloudflare.Client
	var cf store.Cloudflare
	if connected {
		var err error
		if c, cf, err = cfClient(s); err != nil {
			return err
		}
	}
	if connected && domain != "" {
		zones, err := c.Zones()
		if err != nil {
			return err
		}
		zone, ok := cloudflare.ZoneFor(zones, domain)
		if !ok {
			return fmt.Errorf("%s is not in a domain of your Cloudflare account", domain)
		}
		if params.DnsRecordID, err = c.RouteHost(zone.ID, domain, cf.TunnelID); err != nil {
			return err
		}
		params.DnsZoneID = zone.ID
	}
	if connected && app.DnsRecordID != "" {
		if err := c.DeleteRecord(app.DnsZoneID, app.DnsRecordID); err != nil {
			if params.DnsRecordID != "" {
				c.DeleteRecord(params.DnsZoneID, params.DnsRecordID)
			}
			return fmt.Errorf("failed to delete the DNS record of %s: %w", app.Domain, err)
		}
	}
	if err := s.SetAppDomain(ctx(), params); err != nil {
		return err
	}
	if err := SyncTunnel(s); err != nil {
		fmt.Println("tunnel routes not updated (retrying):", err)
	}
	return nil
}

func removeAppDNS(s *store.Store, app store.App) error {
	if app.DnsRecordID == "" || !CloudflareConnected(s) {
		return nil
	}
	c, _, err := cfClient(s)
	if err != nil {
		return err
	}
	return c.DeleteRecord(app.DnsZoneID, app.DnsRecordID)
}
