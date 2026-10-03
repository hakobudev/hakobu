package ops

import (
	"errors"
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

// StartTunnel runs cloudflared for the panel's tunnel, once `hakobu setup`
// has created it, and for each client's, leaving running ones alone.
func StartTunnel(s *store.Store) error {
	var errs []error
	for _, a := range tunnelAccounts(s) {
		if err := startTunnel(s, a); err != nil {
			errs = append(errs, fmt.Errorf("tunnel of %s: %w", a.label(), err))
		}
	}
	// A container made before is on no newer project's edge network yet.
	return errors.Join(append(errs, ensureAllProjectNetworks(s))...)
}

// startTunnel runs the account's cloudflared on the edge networks of its
// projects, or starts it again if it already has the tunnel's current
// token. Only the panel's gets the panel's socket.
func startTunnel(s *store.Store, a cfAccount) error {
	if a.TunnelToken == "" {
		return nil
	}
	env, err := deploy.ContainerEnv(ctx(), a.container())
	if err != nil {
		return err
	}
	if env != nil && env["TUNNEL_TOKEN"] == string(a.TunnelToken) {
		return deploy.StartContainer(ctx(), a.container())
	}
	return runTunnel(s, a, "")
}

// runTunnel (re)creates the account's cloudflared on the edge networks of
// its projects, except the project skip (one being deleted).
func runTunnel(s *store.Store, a cfAccount, skip string) error {
	socketDir := ""
	if a.isPanel() {
		var err error
		if socketDir, err = filepath.Abs(PanelSocketDir); err != nil {
			return err
		}
	}
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return err
	}
	var edges []string
	for _, p := range projects {
		if p.CloudflareAccountID.Int64 == a.ID && p.Name != skip {
			edges = append(edges, projectEdge(p.Name))
		}
	}
	_, err = deploy.RunTunnelContainer(ctx(), a.container(), config.CloudflaredImage, string(a.TunnelToken), a.network(), socketDir, edges)
	return err
}

var (
	ingressMu sync.Mutex
	applied   = map[string]string{} // by tunnel ID: the ingress last sent to Cloudflare
)

// SyncTunnel points every public app's domain at its live container
// through its project's account's tunnel, for each tunnel whose routes
// changed since the last sync.
func SyncTunnel(s *store.Store) error {
	ingressMu.Lock()
	defer ingressMu.Unlock()
	accounts := tunnelAccounts(s)
	if len(accounts) == 0 {
		return nil
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return err
	}
	owners, err := projectAccounts(s)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accounts {
		var rules []cloudflare.IngressRule
		for _, app := range apps {
			if owners[app.ProjectID] == a.ID && app.Domain != "" && app.LivePort > 0 {
				rules = append(rules, cloudflare.IngressRule{Hostname: app.Domain, Service: fmt.Sprintf("http://%s:%d", EdgeAlias(app.Name), app.LivePort)})
			}
		}
		rules = append(rules, cloudflare.IngressRule{Service: a.fallback()})
		key := fmt.Sprint(rules)
		if applied[a.TunnelID] == key {
			continue
		}
		if err := a.Client.SetIngress(a.AccountID, a.TunnelID, rules); err != nil {
			errs = append(errs, fmt.Errorf("tunnel of %s: %w", a.label(), err))
			continue
		}
		applied[a.TunnelID] = key
	}
	return errors.Join(errs...)
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
	for _, z := range zones {
		if client, err := s.GetCloudflareAccountByAccountID(ctx(), z.Account.ID); err == nil {
			return nil, fmt.Errorf("the token sees the Cloudflare account of client %s: the panel's token must be for your own account alone", client.Name)
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
	return cloudflare.Client{Token: string(cf.ApiToken), AccountID: cf.AccountID}, cf, nil
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
	recordID, err := c.RouteHost(zone.Account.ID, zone.ID, host, tunnelID)
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

// SetAppDomain points domain at the tunnel of the app's project's account
// (replacing the app's previous record) or, with domain "", makes the app
// private.
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
	var a cfAccount
	if connected {
		var err error
		if a, err = appAccount(s, app); err != nil {
			return err
		}
	}
	if connected && domain != "" {
		if a.TunnelID == "" {
			return fmt.Errorf("%s has no tunnel yet", a.label())
		}
		zones, err := a.Client.Zones()
		if err != nil {
			return err
		}
		zone, ok := cloudflare.ZoneFor(zones, domain)
		if !ok {
			return fmt.Errorf("%s is not in a domain of %s", domain, a.label())
		}
		if params.DnsRecordID, err = a.Client.RouteHost(a.AccountID, zone.ID, domain, a.TunnelID); err != nil {
			return err
		}
		params.DnsZoneID = zone.ID
	}
	if connected && app.DnsRecordID != "" {
		if err := a.Client.DeleteRecord(app.DnsZoneID, app.DnsRecordID); err != nil {
			if params.DnsRecordID != "" {
				_ = a.Client.DeleteRecord(params.DnsZoneID, params.DnsRecordID)
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
	a, err := appAccount(s, app)
	if err != nil {
		return err
	}
	return a.Client.DeleteRecord(app.DnsZoneID, app.DnsRecordID)
}
