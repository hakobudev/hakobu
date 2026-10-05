package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/node"
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

// EdgeAlias is the app's name on its project's edge network.
func EdgeAlias(app string) string { return node.EdgeAlias(app) }

// A tunnel serves one Cloudflare account's projects on one server: one
// tunnel can't reach two servers, as Cloudflare spreads requests over all
// its connectors. The panel's server's tunnels are the panel's own and
// each client's (tables cloudflare and cloudflare_accounts); a server that
// joined gets one per account when a project there first gets a domain
// (server_tunnels). Only the panel's tunnel on the panel's server reaches
// the panel; every other answers 404 to what isn't an app's domain.
type tunnel struct {
	acct   cfAccount
	server server
	ID     string
	Token  secret.String
	row    int64 // in server_tunnels, 0 on the panel's server
}

func (t tunnel) label() string {
	if t.server.Name == "" {
		return "tunnel of " + t.acct.label()
	}
	return "tunnel of " + t.acct.label() + " on " + t.server.label()
}

// fallback is where the tunnel sends what no app's route matches.
func (t tunnel) fallback() string {
	if t.acct.isPanel() && t.server.Name == "" {
		return panelService
	}
	return "http_status:404"
}

func localTunnel(a cfAccount) tunnel { return tunnel{acct: a, ID: a.TunnelID, Token: a.TunnelToken} }

// allTunnels are the tunnels of every server, the panel's server's first.
func allTunnels(s *store.Store) []tunnel {
	var out []tunnel
	for _, a := range tunnelAccounts(s) {
		out = append(out, localTunnel(a))
	}
	rows, _ := s.ListServerTunnels(ctx())
	for _, r := range rows {
		a, err := accountByID(s, r.CloudflareAccountID)
		if err != nil {
			continue
		}
		out = append(out, tunnel{acct: a, server: server{ID: r.NodeID, Name: r.NodeName}, ID: r.TunnelID, Token: r.TunnelToken, row: r.ID})
	}
	return out
}

// tunnelsOn are the tunnels of the server.
func tunnelsOn(s *store.Store, sv server) []tunnel {
	var out []tunnel
	for _, t := range allTunnels(s) {
		if t.server.ID == sv.ID {
			out = append(out, t)
		}
	}
	return out
}

// accountByID is the account with hakobu's ID id (0: the panel's).
func accountByID(s *store.Store, id int64) (cfAccount, error) {
	if id == 0 {
		return panelAccount(s)
	}
	a, err := s.GetCloudflareAccount(ctx(), id)
	if err != nil {
		return cfAccount{}, err
	}
	return clientAccount(a), nil
}

// place is where a project is: its account's and its server's IDs.
type place struct{ account, server int64 }

func projectPlaces(s *store.Store) (map[int64]place, error) {
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return nil, err
	}
	m := make(map[int64]place, len(projects))
	for _, p := range projects {
		m[p.ID] = place{p.CloudflareAccountID.Int64, p.NodeID.Int64}
	}
	return m, nil
}

// StartTunnel runs the cloudflared of the panel's server's tunnels: the
// panel's, once `hakobu setup` has created it, and each client's, leaving
// running ones alone. A server that joined starts its own as it connects.
func StartTunnel(s *store.Store) error {
	var errs []error
	for _, t := range tunnelsOn(s, server{}) {
		if err := startTunnel(s, t); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.label(), err))
		}
	}
	// A container made before is on no newer project's edge network yet.
	return errors.Join(append(errs, ensureServerNetworks(s, server{}))...)
}

// startTunnel runs the tunnel's cloudflared on its server.
func startTunnel(s *store.Store, t tunnel) error {
	spec, err := tunnelSpec(s, t, "")
	if err != nil {
		return err
	}
	return t.server.node().RunTunnel(ctx(), spec)
}

// tunnelSpec is the tunnel's cloudflared as it should be: on the edge
// networks of its account's projects on its server, except skip (one being
// deleted), and with the panel's socket only for the panel's own tunnel.
func tunnelSpec(s *store.Store, t tunnel, skip string) (node.TunnelSpec, error) {
	spec := node.TunnelSpec{Client: t.acct.Name, Token: string(t.Token)}
	if t.fallback() == panelService {
		var err error
		if spec.PanelSocket, err = filepath.Abs(PanelSocketDir); err != nil {
			return spec, err
		}
	}
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return spec, err
	}
	for _, p := range projects {
		if p.CloudflareAccountID.Int64 == t.acct.ID && p.NodeID.Int64 == t.server.ID && p.Name != skip {
			spec.Projects = append(spec.Projects, p.Name)
		}
	}
	return spec, nil
}

// tunnelSpecs is tunnelSpec for every tunnel of the server.
func tunnelSpecs(s *store.Store, sv server, skip string) ([]node.TunnelSpec, error) {
	var out []node.TunnelSpec
	for _, t := range tunnelsOn(s, sv) {
		spec, err := tunnelSpec(s, t, skip)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// tunnelFor is the tunnel of the project's account on its server, made
// the first time on a server that joined.
func tunnelFor(s *store.Store, p store.Project) (tunnel, error) {
	a, err := projectAccount(s, p)
	if err != nil {
		return tunnel{}, err
	}
	sv, err := projectServer(s, p)
	if err != nil {
		return tunnel{}, err
	}
	if sv.ID == 0 {
		if a.TunnelID == "" {
			return tunnel{}, fmt.Errorf("%s has no tunnel yet", a.label())
		}
		return localTunnel(a), nil
	}
	row, err := s.GetServerTunnel(ctx(), store.GetServerTunnelParams{NodeID: sv.ID, CloudflareAccountID: a.ID})
	if err == nil {
		return tunnel{acct: a, server: sv, ID: row.TunnelID, Token: row.TunnelToken, row: row.ID}, nil
	}
	suffix, err := RandomHex(3)
	if err != nil {
		return tunnel{}, err
	}
	owner := "own"
	if !a.isPanel() {
		owner = a.Name
	}
	t := tunnel{acct: a, server: sv}
	id, token, err := a.Client.CreateTunnel(a.AccountID, "hakobu-"+sv.Name+"-"+owner+"-"+suffix, t.fallback())
	if err != nil {
		return tunnel{}, fmt.Errorf("creating a tunnel for %s: %w", sv.label(), err)
	}
	t.ID, t.Token = id, secret.String(token)
	if err := s.CreateServerTunnel(ctx(), store.CreateServerTunnelParams{NodeID: sv.ID, CloudflareAccountID: a.ID, TunnelID: id, TunnelToken: t.Token}); err != nil {
		return tunnel{}, err
	}
	if row, err := s.GetServerTunnel(ctx(), store.GetServerTunnelParams{NodeID: sv.ID, CloudflareAccountID: a.ID}); err == nil {
		t.row = row.ID
	}
	// Started now if the server is connected, else as it connects.
	if err := startTunnel(s, t); err != nil {
		fmt.Printf("%s not started yet: %v\n", t.label(), err)
	}
	return t, nil
}

var (
	ingressMu sync.Mutex
	applied   = map[string]string{} // by tunnel ID: the ingress last sent to Cloudflare
)

// SyncTunnel points every public app's domain at its live container
// through the tunnel of its project's account on its server, with the
// paths routed to other apps first, for each tunnel whose routes changed
// since the last sync.
func SyncTunnel(s *store.Store) error {
	ingressMu.Lock()
	defer ingressMu.Unlock()
	tunnels := allTunnels(s)
	if len(tunnels) == 0 {
		return nil
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return err
	}
	places, err := projectPlaces(s)
	if err != nil {
		return err
	}
	routes, err := s.ListAllAppRoutes(ctx())
	if err != nil {
		return err
	}
	routesOf := map[string][]store.AppRoute{}
	for _, r := range routes {
		routesOf[r.AppName] = append(routesOf[r.AppName], r)
	}
	live := map[string]int64{}
	for _, app := range apps {
		live[app.Name] = app.LivePort
	}
	var errs []error
	for _, t := range tunnels {
		var rules []cloudflare.IngressRule
		for _, app := range apps {
			if places[app.ProjectID] == (place{t.acct.ID, t.server.ID}) && app.Domain != "" && app.LivePort > 0 {
				rules = append(rules, routeRules(app, routesOf[app.Name], live)...)
				rules = append(rules, cloudflare.IngressRule{Hostname: app.Domain, Service: fmt.Sprintf("http://%s:%d", EdgeAlias(app.Name), app.LivePort)})
			}
		}
		rules = append(rules, cloudflare.IngressRule{Service: t.fallback()})
		key := fmt.Sprint(rules)
		if applied[t.ID] == key {
			continue
		}
		if err := t.acct.Client.SetIngress(t.acct.AccountID, t.ID, rules); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.label(), err))
			continue
		}
		applied[t.ID] = key
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

// zoneGone tells whether the zone has left the account (its domain
// lapsed, say), taking its records along.
func zoneGone(c cloudflare.Client, zoneID string) bool {
	zones, err := c.Zones()
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(zones, func(z cloudflare.Zone) bool { return z.ID == zoneID })
}

// PanelMove is what MovePanel did, and what is left for the owner.
type PanelMove struct {
	Host   string
	Done   []string
	Manual []string
}

// MovePanel gives the panel a new address in the tunnel's account, as when
// its domain lapsed: to is a host, or a domain that keeps the panel's
// subdomain (hakobu.old.com moves to hakobu.new.com). What lived on the
// old domain moves along: the apps' addresses (web.old.com to
// web.new.com) and the domain emails come from. The old address's record
// goes if its domain is still there. The running panel then tells its
// servers and restarts the apps for the new address (tellPanelMoves).
func MovePanel(s *store.Store, to string) (PanelMove, error) {
	var m PanelMove
	c, cf, err := cfClient(s)
	if err != nil {
		return m, err
	}
	if cf.TunnelID == "" {
		return m, fmt.Errorf("the panel has no tunnel yet: run `hakobu setup`")
	}
	to = strings.Trim(strings.ToLower(strings.TrimSpace(to)), ".")
	zones, err := c.Zones()
	if err != nil {
		return m, err
	}
	zone, ok := cloudflare.ZoneFor(zones, to)
	if !ok {
		return m, fmt.Errorf("%s is not in a domain the Cloudflare token sees: add the domain to Cloudflare, or give hakobu a token that covers it (--reconnect)", to)
	}
	if zone.Account.ID != cf.AccountID {
		return m, fmt.Errorf("%s is in another Cloudflare account than the panel's tunnel", zone.Name)
	}
	oldHost, oldDomain := config.PublicHost(), config.AppsDomain()
	host := to
	if sub, ok := strings.CutSuffix(oldHost, "."+oldDomain); ok && to == zone.Name && sub != "" {
		host = sub + "." + zone.Name
	}
	if app, err := s.GetAppByDomain(ctx(), host); err == nil {
		return m, fmt.Errorf("%s is the address of app %s", host, app.Name)
	}

	recordID, err := c.RouteHost(cf.AccountID, zone.ID, host, cf.TunnelID)
	if err != nil {
		return m, err
	}
	if cf.PanelRecordID != "" && cf.PanelRecordID != recordID {
		_ = c.DeleteRecord(cf.PanelZoneID, cf.PanelRecordID) // its domain may have left Cloudflare
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{
		AccountID: cf.AccountID, TunnelID: cf.TunnelID, TunnelToken: cf.TunnelToken, PanelZoneID: zone.ID, PanelRecordID: recordID,
	}); err != nil {
		return m, err
	}
	if err := config.SetPublicHost(host); err != nil {
		return m, err
	}
	if err := config.SetAppsDomain(zone.Name); err != nil {
		return m, err
	}
	m.Host = host

	if oldDomain != "" && oldDomain != zone.Name {
		apps, err := s.ListApps(ctx())
		if err != nil {
			return m, err
		}
		for _, app := range apps {
			domain, ok := movedDomain(app.Domain, oldDomain, zone.Name)
			if !ok {
				continue
			}
			if err := SetAppDomain(s, app, domain); err != nil {
				m.Manual = append(m.Manual, fmt.Sprintf("App %s stays at %s: %v", app.Name, app.Domain, err))
				continue
			}
			m.Done = append(m.Done, fmt.Sprintf("app %s: %s → %s", app.Name, app.Domain, domain))
		}
	}

	if n, err := s.GetNotify(ctx()); err == nil {
		from, moved := movedDomain(n.SenderDomain, oldDomain, zone.Name)
		if n.SenderDomain == oldHost {
			from, moved = "", true // the panel's own address: its new one
		}
		if moved {
			_ = SetupNotifications(s, n.Email, n.SenderName, from) // undoing the old domain's routing may fail with it gone
			if now, err := s.GetNotify(ctx()); err == nil && now.SenderDomain != n.SenderDomain {
				m.Done = append(m.Done, fmt.Sprintf("emails: from %s@%s", now.SenderName, now.SenderDomain))
			} else {
				m.Manual = append(m.Manual, fmt.Sprintf("Emails still come from %s@%s: pick a sender in https://%s/settings#notifications", n.SenderName, n.SenderDomain, host))
			}
		} else if _, ok := cloudflare.ZoneFor(zones, n.SenderDomain); !ok {
			m.Manual = append(m.Manual, fmt.Sprintf("Emails come from %s@%s, a domain gone from Cloudflare: pick a sender in https://%s/settings#notifications", n.SenderName, n.SenderDomain, host))
		} else if err := EnsureWatchdog(s, false); err != nil { // to check the new address
			fmt.Println("the watchdog isn't updated yet:", err)
		}
	}

	if app, err := s.GetGitHubApp(ctx()); err == nil {
		settings := "https://github.com/settings/apps/" + app.Slug
		if err := github.SetWebhookURL(app.AppID, string(app.PrivateKey), "https://"+host+"/webhook/github"); err != nil {
			m.Manual = append(m.Manual, fmt.Sprintf("GitHub App webhook URL: https://%s/webhook/github at %s (hakobu couldn't: %v)", host, settings, err))
		} else {
			m.Done = append(m.Done, "GitHub App webhook URL")
		}
		m.Manual = append(m.Manual, fmt.Sprintf("GitHub App callback URL: https://%s/auth/callback at %s (signing in fails until then)", host, settings))
	}
	return m, nil
}

// movedDomain is domain moved from the old zone to the new one, if it was
// in the old one.
func movedDomain(domain, from, to string) (string, bool) {
	switch {
	case from == "" || domain == "":
		return "", false
	case domain == from:
		return to, true
	}
	if sub, ok := strings.CutSuffix(domain, "."+from); ok {
		return sub + "." + to, true
	}
	return "", false
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
		p, err := s.GetProject(ctx(), app.ProjectName)
		if err != nil {
			return err
		}
		t, err := tunnelFor(s, p)
		if err != nil {
			return err
		}
		zones, err := a.Client.Zones()
		if err != nil {
			return err
		}
		zone, ok := cloudflare.ZoneFor(zones, domain)
		if !ok {
			return fmt.Errorf("%s is not in a domain of %s", domain, a.label())
		}
		if params.DnsRecordID, err = a.Client.RouteHost(a.AccountID, zone.ID, domain, t.ID); err != nil {
			return err
		}
		params.DnsZoneID = zone.ID
	}
	if connected && app.DnsRecordID != "" {
		if err := a.Client.DeleteRecord(app.DnsZoneID, app.DnsRecordID); err != nil && !zoneGone(a.Client, app.DnsZoneID) {
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
