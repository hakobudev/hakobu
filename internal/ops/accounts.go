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
	Name         string // the client's, "" for the panel's
	Client       cloudflare.Client
	AccountID    string
	TunnelID     string
	TunnelToken  secret.String
	BackupBucket string
	OAuth        bool   // connected with "Connect with Cloudflare"
	R2Token      string // an OAuth account's API token for R2, whose S3 keys backups use
}

func (a cfAccount) isPanel() bool { return a.ID == 0 }

// s3Client is the client whose token's S3 keys reach the account's R2: its
// own, or for an OAuth account, whose token has no S3 keys, its R2 token.
func (a cfAccount) s3Client() (cloudflare.Client, error) {
	if !a.OAuth {
		return a.Client, nil
	}
	if a.R2Token == "" {
		return cloudflare.Client{}, fmt.Errorf("backups in Cloudflare account %s need an R2 token: add one in Settings → Cloudflare accounts", a.Name)
	}
	return cloudflare.Client{Token: a.R2Token, AccountID: a.AccountID}, nil
}

func (a cfAccount) label() string {
	if a.isPanel() {
		return "the panel's Cloudflare account"
	}
	return "client " + a.Name + "'s Cloudflare account"
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
		ID: a.ID, Name: a.Name, Client: cloudflare.Client{Token: freshToken(s, a), AccountID: a.AccountID},
		AccountID: a.AccountID, TunnelID: a.TunnelID, TunnelToken: a.TunnelToken, BackupBucket: a.BackupBucket,
		OAuth: a.RefreshToken != "", R2Token: string(a.R2Token),
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
	OAuth     bool   // connected with "Connect with Cloudflare"
	R2Token   bool   // an OAuth account has its R2 token for backups
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
		out[i] = ClientAccount{ID: a.ID, UserID: a.UserID, Name: a.Name, AccountID: a.AccountID, OAuth: a.RefreshToken != "", R2Token: a.R2Token != ""}
		out[i].Projects, _ = s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: a.ID, Valid: true})
	}
	return out, nil
}

// ClientTokenURL opens the form for the token a client makes for hakobu.
func ClientTokenURL(name string) string {
	host, _ := os.Hostname()
	return cloudflare.ClientTokenTemplateURL("hakobu " + strings.Split(host, ".")[0] + " " + name)
}

// R2TokenURL is ClientTokenURL for an OAuth account's R2 token.
func R2TokenURL(name string) string {
	host, _ := os.Hostname()
	return cloudflare.R2TokenTemplateURL("hakobu " + strings.Split(host, ".")[0] + " " + name + " R2")
}

// checkClientToken checks a client's token and returns the one account
// whose domains it sees.
func checkClientToken(token string) (string, error) {
	zones, err := cloudflare.Client{Token: token}.CheckToken()
	if err != nil {
		return "", err
	}
	account := zones[0].Account.ID
	for _, z := range zones {
		if z.Account.ID != account {
			return "", errors.New("the token sees domains of several Cloudflare accounts: make it in the client's account, as an account-owned token")
		}
	}
	return account, nil
}

// AddClientAccount connects a client's Cloudflare account with the token
// they made and creates its tunnel.
func AddClientAccount(s *store.Store, userID int64, name, token string) error {
	if err := checkName("client", name); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	account, err := checkClientToken(token)
	if err != nil {
		return err
	}
	return connectAccount(s, userID, name, account, cloudflare.Client{Token: token, AccountID: account}, func() (int64, error) {
		return s.CreateCloudflareAccount(ctx(), store.CreateCloudflareAccountParams{Name: name, ApiToken: secret.String(token), AccountID: account, UserID: userID})
	})
}

// connectAccount checks a Cloudflare account can be connected as name,
// records it with insert and creates its tunnel with c.
func connectAccount(s *store.Store, userID int64, name, account string, c cloudflare.Client, insert func() (int64, error)) error {
	if panel, err := panelAccount(s); err == nil && panel.AccountID == account {
		return errors.New("that is the panel's own Cloudflare account: its projects need no client account")
	}
	if other, err := s.GetCloudflareAccountByAccountID(ctx(), account); err == nil {
		if other.UserID != userID {
			return errors.New("that Cloudflare account is already connected to this panel by someone else")
		}
		return fmt.Errorf("that Cloudflare account is already connected as %s", other.Name)
	}
	if _, err := s.GetCloudflareAccountByName(ctx(), name); err == nil {
		return fmt.Errorf("a client named %q already exists", name)
	}
	id, err := insert()
	if err != nil {
		return err
	}
	a := cfAccount{ID: id, Name: name, Client: c, AccountID: account}
	host, _ := os.Hostname()
	suffix, err := RandomHex(3)
	if err == nil {
		a.TunnelID, err = createClientTunnel(s, &a, "hakobu-"+strings.Split(host, ".")[0]+"-"+name+"-"+suffix)
	}
	if err != nil {
		_ = s.DeleteCloudflareAccount(ctx(), id)
		return fmt.Errorf("creating the tunnel in the client's account: %w", err)
	}
	return startTunnel(s, localTunnel(a))
}

func createClientTunnel(s *store.Store, a *cfAccount, tunnelName string) (string, error) {
	id, token, err := a.Client.CreateTunnel(a.AccountID, tunnelName, localTunnel(*a).fallback())
	if err != nil {
		return "", err
	}
	a.TunnelToken = secret.String(token)
	return id, s.SetCloudflareAccountTunnel(ctx(), store.SetCloudflareAccountTunnelParams{TunnelID: id, TunnelToken: a.TunnelToken, ID: a.ID})
}

// ReplaceClientToken takes a client's new token, which must be for the
// same account.
func ReplaceClientToken(s *store.Store, name, token string) error {
	row, err := s.GetCloudflareAccountByName(ctx(), name)
	if err != nil {
		return fmt.Errorf("client %q not found", name)
	}
	token = strings.TrimSpace(token)
	account, err := checkClientToken(token)
	if err != nil {
		return err
	}
	if account != row.AccountID {
		return fmt.Errorf("the token is for another Cloudflare account than client %s's", name)
	}
	// An API token in place of OAuth ends the refreshing and takes the
	// account out of its sign-in; its own S3 keys then serve the backups.
	return s.SetCloudflareAccountOAuth(ctx(), store.SetCloudflareAccountOAuthParams{ApiToken: secret.String(token), ID: row.ID})
}

// RemoveClientAccount disconnects a client's account that no project uses
// any more: its cloudflared stops and its tunnel is deleted. The buckets
// and backups in it stay, the client's to keep or delete.
func RemoveClientAccount(s *store.Store, name string) error {
	row, err := s.GetCloudflareAccountByName(ctx(), name)
	if err != nil {
		return fmt.Errorf("client %q not found", name)
	}
	if projects, _ := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: row.ID, Valid: true}); len(projects) > 0 {
		return fmt.Errorf("client %s still has %s: move or delete them first", name, strings.Join(projects, ", "))
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
			panellog.Errorf("failed to delete the tunnel of client %s (delete it in their dashboard): %v", name, err)
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
			return fmt.Errorf("client %q not found", client)
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
