package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
)

// A user connects their Cloudflare accounts with one token of their own,
// made in their Cloudflare profile for every account they're a member of:
// clients invite them to theirs. The token connects every account it
// reaches, and they share it (one token_group) and follow it
// (SyncAccountTokens): an account a client invites the user to since is
// connected, one the user lost is forgotten while no project uses it.
// Nothing is made in an account until a project there needs it.

// The panel's own token is the installer's, made the same way: the first
// admin gets it as theirs when they first sign in (AdoptPanelToken), so
// one token connects the panel's account and their clients'.

// AdoptPanelToken makes the panel's token the user's, connecting the other
// accounts it reaches; once, for the first admin. It doesn't fail the
// sign-in: what goes wrong is in the panel log, and the admin can paste
// the token again under Cloudflare token. A panel-only panel's token
// stays the panel's: it's made for the panel's account alone, and the
// admin connects theirs like anyone else.
func AdoptPanelToken(s *store.Store, userID int64) {
	if config.PanelOnly {
		return
	}
	cf, err := s.GetCloudflare(ctx())
	if err != nil || cf.TokenGroup != "" {
		return
	}
	group, err := RandomHex(16)
	if err == nil {
		err = s.SetCloudflareOwner(ctx(), store.SetCloudflareOwnerParams{UserID: userID, TokenGroup: group})
	}
	if err != nil {
		panellog.Error("making the panel's Cloudflare token the admin's:", err)
		return
	}
	accounts, err := checkAccountToken(string(cf.ApiToken))
	if err != nil {
		panellog.Warn("the panel's Cloudflare token connects no other accounts:", err)
		return
	}
	for _, a := range accounts {
		if why, err := addTokenAccount(s, userID, string(cf.ApiToken), group, a); err != nil {
			panellog.Error("connecting Cloudflare account", accountTitle(a)+":", err)
		} else if why == "" {
			panellog.Info("connected Cloudflare account", accountTitle(a)+" with the panel's token")
		}
	}
}

// AddAccountToken connects every account the token reaches that isn't
// connected yet. A user has one token: one who has it already replaces it
// (ReplaceAccountToken).
func AddAccountToken(s *store.Store, userID int64, token string) error {
	if _, ok := userTokenGroup(s, userID); ok {
		return ReplaceAccountToken(s, userID, token)
	}
	return addAccountToken(s, userID, token)
}

func addAccountToken(s *store.Store, userID int64, token string) error {
	token = strings.TrimSpace(token)
	accounts, err := checkAccountToken(token)
	if err != nil {
		return err
	}
	group, err := RandomHex(16)
	if err != nil {
		return err
	}
	var added int
	var skipped []string
	for _, a := range accounts {
		why, err := addTokenAccount(s, userID, token, group, a)
		if err != nil {
			return err
		}
		if why != "" {
			skipped = append(skipped, accountTitle(a)+": "+why)
		} else {
			added++
		}
	}
	if added == 0 {
		return fmt.Errorf("nothing new to connect (%s); to change the token of accounts you connected, use Replace token", strings.Join(skipped, "; "))
	}
	return nil
}

// checkAccountToken lists the accounts the token reaches and checks it
// can do what hakobu needs in each: manage their tunnels and R2. A token
// lacking either says so now, not with a project's first domain or backup.
func checkAccountToken(token string) ([]cloudflare.Account, error) {
	c := cloudflare.Client{Token: token}
	accounts, err := c.Accounts()
	if err != nil {
		return nil, fmt.Errorf("the token doesn't work: %w", err)
	}
	if len(accounts) == 0 {
		return nil, errors.New("the token sees no domains: give it Zone Read and DNS Edit, for all zones")
	}
	for _, a := range accounts {
		c.AccountID = a.ID
		if err := c.CanCreateTunnels(a.ID); err != nil {
			return nil, fmt.Errorf("in %s the token can't create tunnels: give it Cloudflare Tunnel Edit, for all accounts (%w)", accountTitle(a), err)
		}
		if err := c.CanUseR2(a.ID); err != nil {
			return nil, fmt.Errorf("in %s the token can't use R2: give it Workers R2 Storage Edit, for all accounts (%w)", accountTitle(a), err)
		}
	}
	return accounts, nil
}

// addTokenAccount connects account a with the token of group, unless it's
// the panel's or connected already, which it says instead.
func addTokenAccount(s *store.Store, userID int64, token, group string, a cloudflare.Account) (string, error) {
	if panel, err := panelAccount(s); err == nil && panel.AccountID == a.ID {
		return "the panel's own account", nil
	}
	if other, err := s.GetCloudflareAccountByAccountID(ctx(), a.ID); err == nil {
		if other.UserID != userID {
			return "connected by someone else", nil
		}
		return "connected already", nil
	}
	_, err := s.CreateCloudflareAccount(ctx(), store.CreateCloudflareAccountParams{
		Name: accountKey(s, a), UserID: userID, ApiToken: secret.String(token), TokenGroup: group,
		AccountID: a.ID, Title: a.Name, Domains: strings.Join(a.Zones, ", "),
	})
	return "", err
}

// SyncAccountTokens has each user's token connect the accounts it reaches
// now: ones new to it are connected, ones it lost are forgotten if no
// project uses them and otherwise told to their owner. A token that
// stopped working is told too, and changes nothing.
func SyncAccountTokens(s *store.Store) {
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return
	}
	seen := map[string]bool{}
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.TokenGroup != "" {
		seen[cf.TokenGroup] = true
		syncTokenGroup(s, cf.TokenGroup)
	}
	for _, r := range rows {
		if !seen[r.TokenGroup] {
			seen[r.TokenGroup] = true
			syncTokenGroup(s, r.TokenGroup)
		}
	}
}

// WatchCloudflareAccounts runs SyncAccountTokens every interval.
func WatchCloudflareAccounts(s *store.Store, interval time.Duration) {
	for {
		SyncAccountTokens(s)
		time.Sleep(interval)
	}
}

// userTokenGroup is the group of the user's token, if they have one.
func userTokenGroup(s *store.Store, userID int64) (string, bool) {
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.TokenGroup != "" && cf.UserID == userID {
		return cf.TokenGroup, true
	}
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return "", false
	}
	for _, r := range rows {
		if r.UserID == userID {
			return r.TokenGroup, true
		}
	}
	return "", false
}

// tokenGroup is the accounts sharing the token of group.
func tokenGroup(s *store.Store, group string) []store.CloudflareAccount {
	var out []store.CloudflareAccount
	if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
		for _, r := range rows {
			if r.TokenGroup == group {
				out = append(out, r)
			}
		}
	}
	return out
}

func syncTokenGroup(s *store.Store, group string) {
	rows := tokenGroup(s, group)
	var token string
	var userID int64
	var titles []string
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.TokenGroup == group {
		// The panel's token problem is told by its own check.
		token, userID = string(cf.ApiToken), cf.UserID
	} else if len(rows) > 0 {
		token, userID = string(rows[0].ApiToken), rows[0].UserID
	} else {
		return
	}
	for _, r := range rows {
		titles = append(titles, r.Title)
	}
	who := strings.Join(titles, ", ")
	key := "cloudflare-token:" + group
	accounts, err := cloudflare.Client{Token: token}.Accounts()
	if err != nil {
		panellog.Error("the Cloudflare token of", who, "doesn't work:", err)
		problem(s, key, notifyAgain, "Your Cloudflare token stopped working",
			"hakobu can't use the token of your Cloudflare accounts ("+who+"): "+err.Error()+"\n\nGive it a new one: Settings → Cloudflare → Replace token.\n\n"+panelURL("/settings#cloudflare"))
		return
	}
	solved(s, key, "Your Cloudflare token works again", "hakobu can use it again.")
	reaches := map[string]cloudflare.Account{}
	for _, a := range accounts {
		reaches[a.ID] = a
	}
	for _, r := range rows {
		a, ok := reaches[r.AccountID]
		if ok {
			if domains := strings.Join(a.Zones, ", "); a.Name != r.Title || domains != r.Domains {
				_ = s.SetCloudflareAccountInfo(ctx(), store.SetCloudflareAccountInfoParams{Title: a.Name, Domains: domains, ID: r.ID})
			}
			delete(reaches, r.AccountID)
			continue
		}
		lost := "cloudflare-lost:" + r.AccountID
		if projects, _ := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: r.ID, Valid: true}); len(projects) > 0 {
			problem(s, lost, notifyAgain, "hakobu lost Cloudflare account "+r.Title,
				"Your token no longer reaches Cloudflare account "+r.Title+" ("+r.Domains+"), which "+strings.Join(projects, ", ")+" use: its owner may have removed you from it. Their sites keep working, but hakobu can't change their domains or back them up there.\n\n"+panelURL("/settings#cloudflare"))
			continue
		}
		if err := RemoveClientAccount(s, r.Name); err != nil {
			panellog.Error("forgetting Cloudflare account", r.Title+", which the token no longer reaches:", err)
		} else {
			panellog.Info("forgot Cloudflare account", r.Title+": the token no longer reaches it")
		}
	}
	for _, a := range reaches {
		if why, err := addTokenAccount(s, userID, token, group, a); err != nil {
			panellog.Error("connecting Cloudflare account", accountTitle(a)+":", err)
		} else if why == "" {
			panellog.Info("connected Cloudflare account", accountTitle(a)+", which the token reaches now")
		}
	}
}

// ReplaceAccountToken puts a new token in place of the one the user's
// accounts share; it must reach those that projects use, and the ones it
// doesn't reach are forgotten.
func ReplaceAccountToken(s *store.Store, userID int64, token string) error {
	group, ok := userTokenGroup(s, userID)
	if !ok {
		return addAccountToken(s, userID, token)
	}
	token = strings.TrimSpace(token)
	accounts, err := checkAccountToken(token)
	if err != nil {
		return err
	}
	reaches := map[string]bool{}
	for _, a := range accounts {
		reaches[a.ID] = true
	}
	var missing []string
	for _, r := range tokenGroup(s, group) {
		if reaches[r.AccountID] {
			continue
		}
		if projects, _ := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: r.ID, Valid: true}); len(projects) > 0 {
			missing = append(missing, r.Title)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the token doesn't reach %s, which projects use: make it for all accounts", strings.Join(missing, ", "))
	}
	if cf, err := s.GetCloudflare(ctx()); err == nil && cf.TokenGroup == group {
		// It's the panel's too: that saves it for the whole group.
		if err := replacePanelToken(s, token); err != nil {
			return err
		}
	} else if err := s.SetTokenGroupToken(ctx(), store.SetTokenGroupTokenParams{ApiToken: secret.String(token), TokenGroup: group}); err != nil {
		return err
	}
	syncTokenGroup(s, group)
	return nil
}

func accountTitle(a cloudflare.Account) string {
	if len(a.Zones) > 0 {
		return a.Name + " (" + a.Zones[0] + ")"
	}
	return a.Name
}

// accountKey is hakobu's own key for a connected account, never shown:
// made of its first domain (kaslauto.com: kaslauto), unique.
func accountKey(s *store.Store, a cloudflare.Account) string {
	from := a.Name
	if len(a.Zones) > 0 {
		from, _, _ = strings.Cut(a.Zones[0], ".")
	}
	key := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(from), "-"), "-")
	if key == "" || key[0] < 'a' || key[0] > 'z' {
		key = "cf-" + key
	}
	if len(key) > 30 {
		key = strings.TrimRight(key[:30], "-")
	}
	base := key
	for i := 2; ; i++ {
		if _, err := s.GetCloudflareAccountByName(ctx(), key); err != nil {
			return key
		}
		key = fmt.Sprintf("%s-%d", base, i)
	}
}
