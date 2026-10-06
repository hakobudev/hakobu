package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Cloudflare accounts. The panel's (table cloudflare, connected by
// `hakobu setup`) holds the panel and the owner's own projects. A client's
// (table cloudflare_accounts) holds the domains, R2 storages and backups of
// the projects assigned to it, reached through a tunnel of its own.
//
// The client owns that tunnel and can change its routes in their
// dashboard, so its cloudflared gets no panel socket and joins only the
// edge networks of the client's projects: whatever the routes say, it
// reaches nothing else of hakobu's.

type cfAccount struct {
	ID           int64  // 0 for the panel's
	Name         string // hakobu's key for it, "" for the panel's
	Title        string // what Cloudflare calls it
	Client       cloudflare.Client
	AccountID    string
	TunnelID     string
	TunnelToken  secret.String
	BackupBucket string
}

func (a cfAccount) isPanel() bool { return a.ID == 0 }

func (a cfAccount) label() string {
	if a.isPanel() {
		return "the panel's Cloudflare account"
	}
	return "Cloudflare account " + a.Title
}

// container runs the account's cloudflared.
func (a cfAccount) container() string { return node.TunnelName(a.Name) }

func panelAccount(s *store.Store) (cfAccount, error) {
	c, cf, err := cfClient(s)
	if err != nil {
		return cfAccount{}, err
	}
	return cfAccount{Client: c, AccountID: cf.AccountID, TunnelID: cf.TunnelID, TunnelToken: cf.TunnelToken, BackupBucket: cf.BackupBucket}, nil
}

func clientAccount(s *store.Store, a store.CloudflareAccount) cfAccount {
	return cfAccount{
		ID: a.ID, Name: a.Name, Title: a.Title, Client: cloudflare.Client{Token: string(a.ApiToken), AccountID: a.AccountID},
		AccountID: a.AccountID, TunnelID: a.TunnelID, TunnelToken: a.TunnelToken, BackupBucket: a.BackupBucket,
	}
}

func projectAccount(s *store.Store, p store.Project) (cfAccount, error) {
	if !p.CloudflareAccountID.Valid {
		return panelAccount(s)
	}
	a, err := s.GetCloudflareAccount(ctx(), p.CloudflareAccountID.Int64)
	if err != nil {
		return cfAccount{}, fmt.Errorf("the Cloudflare account of project %s: %w", p.Name, err)
	}
	return clientAccount(s, a), nil
}

func projectAccountByName(s *store.Store, project string) (cfAccount, error) {
	p, err := s.GetProject(ctx(), project)
	if err != nil {
		return cfAccount{}, fmt.Errorf("project %q not found: %w", project, err)
	}
	return projectAccount(s, p)
}

func appAccount(s *store.Store, app store.App) (cfAccount, error) {
	return projectAccountByName(s, app.ProjectName)
}

// accountByCloudflareID finds the connected account with Cloudflare's
// account ID id; "" is the panel's, where backups went before clients.
func accountByCloudflareID(s *store.Store, id string) (cfAccount, error) {
	panel, err := panelAccount(s)
	if id == "" || (err == nil && panel.AccountID == id) {
		return panel, err
	}
	a, err := s.GetCloudflareAccountByAccountID(ctx(), id)
	if err != nil {
		return cfAccount{}, fmt.Errorf("Cloudflare account %s isn't connected any more", id)
	}
	return clientAccount(s, a), nil
}

// tunnelAccounts are the accounts with a tunnel, the panel's first.
func tunnelAccounts(s *store.Store) []cfAccount {
	var out []cfAccount
	if p, err := panelAccount(s); err == nil && p.TunnelID != "" {
		out = append(out, p)
	}
	clients, _ := s.ListCloudflareAccounts(ctx())
	for _, c := range clients {
		if c.TunnelID != "" {
			out = append(out, clientAccount(s, c))
		}
	}
	return out
}

// Clients' accounts, as the panel manages them.

// ClientAccount is a client's Cloudflare account and its projects.
type ClientAccount struct {
	ID        int64
	UserID    int64 // who connected it
	Name      string
	AccountID string
	Projects  []string
	R2Off     bool   // known only after CheckClientsR2
	R2URL     string // where the client turns R2 on
	Title     string // what Cloudflare calls it
	Domains   string
	Panel     bool // the panel's own (PanelClientAccount)
}

// CheckClientsR2 tells which clients haven't turned R2 on (R2Off).
func CheckClientsR2(s *store.Store, clients []ClientAccount) {
	for i, c := range clients {
		a, err := s.GetCloudflareAccount(ctx(), c.ID)
		if err != nil {
			continue
		}
		clients[i].R2Off, clients[i].R2URL = R2Off(clientAccount(s, a).Client, a.AccountID), cloudflare.R2URL(a.AccountID)
	}
}

func ClientAccounts(s *store.Store) ([]ClientAccount, error) {
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return nil, err
	}
	out := make([]ClientAccount, len(rows))
	for i, a := range rows {
		out[i] = ClientAccount{ID: a.ID, UserID: a.UserID, Name: a.Name, AccountID: a.AccountID, Title: a.Title, Domains: a.Domains}
		out[i].Projects, _ = s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: a.ID, Valid: true})
	}
	return out, nil
}

// PanelClientAccount is the panel's account as a user's list of accounts
// shows it, with their projects in it.
func PanelClientAccount(s *store.Store, userID int64) (ClientAccount, error) {
	a, err := panelAccount(s)
	if err != nil {
		return ClientAccount{}, err
	}
	out := ClientAccount{AccountID: a.AccountID, Panel: true}
	if zones, err := a.Client.Zones(); err == nil && len(zones) > 0 {
		out.Title = zones[0].Account.Name
		names := make([]string, len(zones))
		for i, z := range zones {
			names[i] = z.Name
		}
		out.Domains = strings.Join(names, ", ")
	}
	out.Projects, _ = s.UserProjectsInPanelAccount(ctx(), userID)
	return out, nil
}

// OwnsPanelToken tells whether the panel's token is the user's too.
func OwnsPanelToken(s *store.Store, userID int64) bool {
	cf, err := s.GetCloudflare(ctx())
	return err == nil && cf.TokenGroup != "" && cf.UserID == userID
}

// UserTokenURL opens the form for a token of the user's own for all the
// accounts they're a member of, clients' that invited them included.
func UserTokenURL() string {
	host, _ := os.Hostname()
	return cloudflare.UserTokenTemplateURL("hakobu " + strings.Split(host, ".")[0])
}

func createClientTunnel(s *store.Store, a *cfAccount, tunnelName string) (string, error) {
	id, token, err := a.Client.CreateTunnel(a.AccountID, tunnelName, localTunnel(*a).fallback())
	if err != nil {
		return "", err
	}
	a.TunnelToken = secret.String(token)
	return id, s.SetCloudflareAccountTunnel(ctx(), store.SetCloudflareAccountTunnelParams{TunnelID: id, TunnelToken: a.TunnelToken, ID: a.ID})
}

// RemoveClientAccount disconnects an account that no project uses any
// more: its cloudflared stops and its tunnels are deleted. The buckets and
// backups in it stay, its owner's to keep or delete.
func RemoveClientAccount(s *store.Store, name string) error {
	row, err := s.GetCloudflareAccountByName(ctx(), name)
	if err != nil {
		return errors.New("Cloudflare account not found")
	}
	if projects, _ := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: row.ID, Valid: true}); len(projects) > 0 {
		return fmt.Errorf("Cloudflare account %s still has %s: move or delete them first", row.Title, strings.Join(projects, ", "))
	}
	a := clientAccount(s, row)
	if err := local.RemoveTunnel(ctx(), a.Name); err != nil {
		return err
	}
	// Its tunnels on other servers serve no project any more either.
	for _, t := range allTunnels(s) {
		if t.row != 0 && t.acct.ID == a.ID {
			dropServerTunnel(s, t)
		}
	}
	if a.TunnelID != "" {
		// A revoked token mustn't keep the client connected here; the
		// tunnel left behind has no connector and routes nothing of ours.
		if err := a.Client.DeleteTunnel(a.AccountID, a.TunnelID); err != nil {
			panellog.Errorf("failed to delete the tunnel in Cloudflare account %s (delete it in its dashboard): %v", row.Title, err)
		}
	}
	return s.DeleteCloudflareAccount(ctx(), row.ID)
}

// SetProjectAccount moves a project to a client's account, or with client
// "" to the panel's. Its apps' domains are in the old account, so a project
// with public apps must make them private first.
func SetProjectAccount(s *store.Store, project, client string) error {
	p, err := s.GetProject(ctx(), project)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", project, err)
	}
	if config.PanelOnly && client == "" {
		return errPanelOnly
	}
	var id sql.NullInt64
	if client != "" {
		a, err := s.GetCloudflareAccountByName(ctx(), client)
		if err != nil || a.UserID != p.UserID {
			return errors.New("Cloudflare account not found")
		}
		id = sql.NullInt64{Int64: a.ID, Valid: true}
	}
	if id == p.CloudflareAccountID {
		return nil
	}
	apps, err := s.ListAppsByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	var public []string
	for _, a := range apps {
		if a.Domain != "" {
			public = append(public, a.Name)
		}
	}
	if len(public) > 0 {
		return fmt.Errorf("%s %s a domain in the current account: make %s private first", strings.Join(public, ", "), plural(len(public), "has", "have"), plural(len(public), "it", "them"))
	}
	if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: id, ID: p.ID}); err != nil {
		return err
	}
	return ensureProjectNetworks(s, project)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ProjectZones are the domains of the project's account and the one a new
// app's address defaults to.
func ProjectZones(s *store.Store, project string) (zones []string, def string, err error) {
	a, err := projectAccountByName(s, project)
	if err != nil {
		return nil, "", err
	}
	list, err := a.Client.Zones()
	if err != nil {
		return nil, "", err
	}
	for _, z := range list {
		zones = append(zones, z.Name)
	}
	if a.isPanel() {
		return zones, config.AppsDomain(), nil
	}
	if len(zones) > 0 {
		def = zones[0]
	}
	return zones, def, nil
}

// RemoveUser takes a user's access away: their sessions and AI apps end.
// Only someone who owns nothing any more can be removed: the panel's admin
// doesn't see others' projects, so they remove their own first.
func RemoveUser(s *store.Store, id int64) error {
	u, err := s.GetUser(ctx(), id)
	if err != nil {
		return err
	}
	if u.Admin == 1 {
		return errors.New("an admin can't be removed: make them a regular user first")
	}
	var owns []string
	if ps, err := s.ListProjectsOf(ctx(), id); err == nil && len(ps) > 0 {
		owns = append(owns, fmt.Sprintf("%d %s", len(ps), plural(len(ps), "project", "projects")))
	}
	if n, err := s.CountNodesOf(ctx(), id); err == nil && n > 0 {
		owns = append(owns, fmt.Sprintf("%d %s", n, plural(int(n), "server", "servers")))
	}
	if n, err := s.CountCloudflareAccountsOf(ctx(), id); err == nil && n > 0 {
		owns = append(owns, fmt.Sprintf("%d Cloudflare %s", n, plural(int(n), "account", "accounts")))
	}
	if len(owns) > 0 {
		return fmt.Errorf("%s still has %s: they remove them first", u.GitHubLogin, strings.Join(owns, ", "))
	}
	if err := s.DeleteOAuthGrantsOf(ctx(), u.GitHubID); err != nil {
		return err
	}
	if err := s.DeleteSessionsOf(ctx(), u.GitHubID); err != nil {
		return err
	}
	return s.DeleteUser(ctx(), id)
}

// SetUserAdmin makes a user one of the panel's admins, or no longer; the
// panel keeps at least one.
func SetUserAdmin(s *store.Store, id int64, admin bool) error {
	u, err := s.GetUser(ctx(), id)
	if err != nil {
		return err
	}
	flag := int64(0)
	if admin {
		flag = 1
	}
	if u.Admin == flag {
		return nil
	}
	if !admin {
		if n, err := s.CountAdmins(ctx()); err != nil || n <= 1 {
			return errors.New("the panel needs at least one admin: make someone else one first")
		}
	}
	return s.SetUserAdmin(ctx(), store.SetUserAdminParams{Admin: flag, ID: id})
}
