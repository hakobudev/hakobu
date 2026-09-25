package ops

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/tunnel"
)

// Tunnel is the cloudflared process serving the panel and the apps.
var Tunnel = &tunnel.Runner{}

const panelService = "http://127.0.0.1:9000"

// StartTunnel runs the tunnel once `hakobu setup` has created it.
func StartTunnel(s *store.Store) {
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.TunnelToken != "" {
		Tunnel.Named(cf.TunnelToken)
	}
}

func CloudflareConnected(s *store.Store) bool {
	_, err := s.GetCloudflare(ctx())
	return err == nil
}

func TunnelReady(s *store.Store) bool {
	cf, err := s.GetCloudflare(ctx())
	return err == nil && cf.TunnelID != ""
}

func SaveCloudflareToken(s *store.Store, t cloudflare.Token) error {
	return s.SaveCloudflareToken(ctx(), store.SaveCloudflareTokenParams{
		AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: t.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// refreshMu serializes token refreshes: refresh tokens rotate, and using
// one twice can get the whole grant revoked.
var refreshMu sync.Mutex

// cfClient returns an API client, refreshing the access token when it's
// about to expire.
func cfClient(s *store.Store) (cloudflare.Client, store.Cloudflare, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	cf, err := s.GetCloudflare(ctx())
	if err != nil {
		return cloudflare.Client{}, cf, fmt.Errorf("Cloudflare is not connected")
	}
	exp, _ := time.Parse(time.RFC3339, cf.ExpiresAt)
	if time.Until(exp) < time.Minute {
		t, err := cloudflare.Refresh(config.CloudflareClientID, cf.RefreshToken)
		if err != nil {
			return cloudflare.Client{}, cf, fmt.Errorf("Cloudflare access expired; reconnect with `cd /opt/hakobu && sudo ./hakobu setup --reconnect`: %w", err)
		}
		if t.RefreshToken == "" {
			t.RefreshToken = cf.RefreshToken
		}
		if err := SaveCloudflareToken(s, t); err != nil {
			return cloudflare.Client{}, cf, err
		}
		cf.AccessToken = t.AccessToken
	}
	return cloudflare.Client{Token: cf.AccessToken}, cf, nil
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
		AccountID: zone.Account.ID, TunnelID: tunnelID, TunnelToken: token, PanelZoneID: zone.ID, PanelRecordID: recordID,
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
	return s.SetAppDomain(ctx(), params)
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
