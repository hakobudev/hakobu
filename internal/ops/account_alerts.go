package ops

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// A client's Cloudflare account alerts its owner about the projects in it
// from itself: emails go from alerts@mail.<a domain of the account> to the
// owner's address, confirmed in that account, and a watchdog Worker there
// checks its apps from outside. The panel's account confirms at most 200
// addresses, which a panel open to everyone would run out of, and the
// panel being down doesn't stop the watchdogs.
//
// Nothing is set up in an account until a project is there and its owner
// has an address (an admin's is the panel's unless they set one); the
// watchdog waits for an app with an address. What hakobu set up goes when
// the account has no projects, its owner no address, or it's disconnected.

// accountAlerts keeps two setups of one account from racing.
var accountAlerts sync.Mutex

// accountAlertsErr is why an account's alerts aren't set up, by its ID.
var accountAlertsErr sync.Map

// accountPerms caches each account's token check, by its ID.
var accountPerms struct {
	sync.Mutex
	at    map[int64]time.Time
	perms map[int64][]cloudflare.Permission
}

// EnsureAccountAlerts sets up, updates or removes the alerts of every
// client's account; an account whose setup fails says why in Settings.
func EnsureAccountAlerts(s *store.Store) {
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return
	}
	for _, r := range rows {
		_ = ensureAccountAlerts(s, r) // Settings shows why
	}
	// Ones of accounts that were forgotten without RemoveClientAccount.
	alerts, _ := s.ListAccountAlerts(ctx())
	for _, al := range alerts {
		if _, err := s.GetCloudflareAccount(ctx(), al.CloudflareAccountID); errors.Is(err, sql.ErrNoRows) {
			_ = s.DeleteAccountAlerts(ctx(), al.CloudflareAccountID)
		}
	}
}

// ensureUserAccountAlerts does it for the user's accounts, telling what
// failed.
func ensureUserAccountAlerts(s *store.Store, userID int64) error {
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if r.UserID != userID {
			continue
		}
		if err := ensureAccountAlerts(s, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Title, err))
		}
	}
	return errors.Join(errs...)
}

func ensureAccountAlerts(s *store.Store, row store.CloudflareAccount) error {
	accountAlerts.Lock()
	defer accountAlerts.Unlock()
	err := setUpAccountAlerts(s, row)
	if err != nil {
		accountAlertsErr.Store(row.ID, err.Error())
		panellog.Warn("alerts from Cloudflare account", row.Title+":", err)
	} else {
		accountAlertsErr.Delete(row.ID)
	}
	return err
}

func setUpAccountAlerts(s *store.Store, row store.CloudflareAccount) error {
	a := clientAccount(s, row)
	projects, err := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: row.ID, Valid: true})
	if err != nil {
		return err
	}
	to := accountRecipient(s, row)
	if len(projects) == 0 || to == "" {
		return removeAccountAlerts(s, a)
	}
	al, changed, err := ensureAccountEmail(s, a, to)
	if err != nil {
		return err
	}
	return ensureAccountWatchdog(s, a, al, changed)
}

// accountRecipient is where alerts about an account's projects go: its
// owner's address.
func accountRecipient(s *store.Store, row store.CloudflareAccount) string {
	u, err := s.GetUser(ctx(), row.UserID)
	if err != nil {
		return ""
	}
	return userAddress(s, u)
}

// userAddress is where a user's alerts go: their own address, or the
// panel's for an admin without one; "" for none.
func userAddress(s *store.Store, u store.User) string {
	if u.NotifyEmail != "" {
		return u.NotifyEmail
	}
	if u.Admin == 1 {
		if n, err := s.GetNotify(ctx()); err == nil {
			return n.Email
		}
	}
	return ""
}

// ensureAccountEmail has the account send to to: a domain of it picked to
// send from, once, and the address added to be confirmed there. changed
// says the address is new.
func ensureAccountEmail(s *store.Store, a cfAccount, to string) (al store.AccountAlert, changed bool, err error) {
	prev, err := s.GetAccountAlerts(ctx(), a.ID)
	had := err == nil
	if had && strings.EqualFold(prev.Email, to) {
		return prev, false, nil
	}
	if lacking := lacks(accountPermissions(a, prev.ZoneID), "the address emails go to"); lacking != "" {
		return prev, false, fmt.Errorf("the token lacks %s: %s", lacking, tokenAdvice)
	}
	sender, zoneID, routed := prev.SenderDomain, prev.ZoneID, prev.RoutedDomain
	if !had {
		if sender, zoneID, routed, err = accountSender(a); err != nil {
			return prev, false, accountHint(err)
		}
	}
	added, err := a.Client.AddDestination(a.AccountID, to)
	if err != nil {
		return prev, false, accountHint(err)
	}
	if had && prev.AddedAddress == 1 && !strings.EqualFold(prev.Email, to) {
		if err := a.Client.DeleteDestination(a.AccountID, prev.Email); err != nil {
			panellog.Warn("removing", prev.Email, "from the addresses of Cloudflare account", a.Title+":", err)
		}
	}
	p := store.SaveAccountEmailParams{CloudflareAccountID: a.ID, Email: to, SenderDomain: sender, ZoneID: zoneID, RoutedDomain: routed}
	if added {
		p.AddedAddress = 1
	}
	if err := s.SaveAccountEmail(ctx(), p); err != nil {
		return prev, false, err
	}
	al, err = s.GetAccountAlerts(ctx(), a.ID)
	return al, true, err
}

// accountSender picks the domain the account's emails come from:
// mail.<zone> of a zone with Email Routing on, which needs nothing more,
// or else of its first zone, with Email Routing turned on for that
// subdomain (or alerts.<zone>, if mail.<zone> gets mail of its own), which
// leaves the domain's own mail alone.
func accountSender(a cfAccount) (sender, zoneID, routed string, err error) {
	zones, err := a.Client.Zones()
	if err != nil {
		return "", "", "", err
	}
	if len(zones) == 0 {
		return "", "", "", errors.New("the account has no domain to send from")
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	for _, z := range zones {
		r, err := a.Client.EmailRouting(z.ID)
		if err != nil {
			return "", "", "", err
		}
		if r.Enabled {
			return "mail." + z.Name, z.ID, "", nil
		}
	}
	z := zones[0]
	for _, name := range []string{"mail." + z.Name, "alerts." + z.Name} {
		mx, err := a.Client.MXRecords(z.ID, name)
		if err != nil {
			return "", "", "", err
		}
		if len(mx) > 0 {
			continue
		}
		if err := enableRouting(a.Client, z, name); err != nil {
			return "", "", "", err
		}
		return name, z.ID, name, nil
	}
	return "", "", "", fmt.Errorf("mail.%s and alerts.%s get mail elsewhere: hakobu can't send from them", z.Name, z.Name)
}

// ensureAccountWatchdog has the account's watchdog check its apps, deployed
// again when they or the address changed, or it's gone; with no app to
// check, there's none.
func ensureAccountWatchdog(s *store.Store, a cfAccount, al store.AccountAlert, redeploy bool) error {
	apps, err := appTargets(s, a.ID)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return removeAccountWatchdog(s, a, al)
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return err
	}
	targets := string(b)
	perms := accountPermissions(a, al.ZoneID)
	if lacking := lacks(perms, "the watchdog"); lacking != "" {
		return fmt.Errorf("no watchdog: the token lacks %s: %s", lacking, tokenAdvice)
	}
	withHistory := canKeepHistory(perms)
	if al.Script != "" && !redeploy && al.Targets == targets && (al.D1DatabaseID != "") == withHistory {
		if exists, err := a.Client.WorkerExists(a.AccountID, al.Script); err != nil || exists {
			return err
		}
		panellog.Warn("the watchdog's Worker is gone from Cloudflare account", a.Title+": deploying it again")
	}
	name := al.Script
	if name == "" {
		name = watchdogName()
	}
	kv, d1, err := deployWatchdog(a.Client, a.AccountID, name, al.Email, DefaultSenderName+"@"+al.SenderDomain, targets, withHistory)
	if err != nil {
		return accountHint(err)
	}
	if al.D1DatabaseID != "" && d1 == "" {
		_ = a.Client.DeleteD1(a.AccountID, al.D1DatabaseID)
	}
	return s.SaveAccountWatchdog(ctx(), store.SaveAccountWatchdogParams{Script: name, KvNamespaceID: kv, Targets: targets, D1DatabaseID: d1, CloudflareAccountID: a.ID})
}

func removeAccountWatchdog(s *store.Store, a cfAccount, al store.AccountAlert) error {
	if al.Script == "" {
		return nil
	}
	if err := removeWatchdog(a.Client, a.AccountID, al.Script, al.KvNamespaceID, al.D1DatabaseID); err != nil {
		return accountHint(err)
	}
	return s.SaveAccountWatchdog(ctx(), store.SaveAccountWatchdogParams{CloudflareAccountID: a.ID})
}

// removeAccountAlerts removes from the account what hakobu set up there
// for alerts: the watchdog, the address if hakobu added it, and Email
// Routing for the subdomain it turned it on for.
func removeAccountAlerts(s *store.Store, a cfAccount) error {
	al, err := s.GetAccountAlerts(ctx(), a.ID)
	if err != nil {
		return nil
	}
	if err := removeAccountWatchdog(s, a, al); err != nil {
		return err
	}
	n := store.Notify{Email: al.Email, AddedAddress: al.AddedAddress, ZoneID: al.ZoneID, RoutedDomain: al.RoutedDomain}
	if err := undoNotify(a.Client, a.AccountID, n, "", ""); err != nil {
		return accountHint(err)
	}
	return s.DeleteAccountAlerts(ctx(), a.ID)
}

// accountPermissions checks what the account's token can do there, at
// most every ten minutes.
func accountPermissions(a cfAccount, zoneID string) []cloudflare.Permission {
	accountPerms.Lock()
	if time.Since(accountPerms.at[a.ID]) < tokenChecked {
		defer accountPerms.Unlock()
		return accountPerms.perms[a.ID]
	}
	accountPerms.Unlock()
	if zoneID == "" {
		if zones, err := a.Client.Zones(); err == nil && len(zones) > 0 {
			zoneID = zones[0].ID
		}
	}
	perms := a.Client.CheckPermissions(a.AccountID, zoneID)
	accountPerms.Lock()
	if accountPerms.at == nil {
		accountPerms.at, accountPerms.perms = map[int64]time.Time{}, map[int64][]cloudflare.Permission{}
	}
	accountPerms.at[a.ID], accountPerms.perms[a.ID] = time.Now(), perms
	accountPerms.Unlock()
	return perms
}

// lacks names the permissions for what the token is missing, "" if none.
func lacks(perms []cloudflare.Permission, what string) string {
	var names []string
	for _, p := range cloudflare.Lacking(perms) {
		if p.For == what {
			names = append(names, p.Name)
		}
	}
	return strings.Join(names, " and ")
}

// tokenAdvice is how a user gives hakobu what their token lacks.
const tokenAdvice = "make a new one with the link in Settings → Cloudflare and Replace token"

// accountHint names the permissions alerts need when Cloudflare refused a
// user's token: one made before hakobu sent alerts from users' accounts
// lacks them.
func accountHint(err error) error {
	if err == nil || !strings.Contains(err.Error(), cloudflare.TokenLacksPermission) {
		return err
	}
	return fmt.Errorf("%w; alerts need Zone Settings Edit, Email Routing Addresses Edit, Email Sending Edit, Workers Scripts Edit, Workers KV Storage Edit and D1 Edit: %s", err, tokenAdvice)
}

// mailer sends from one Cloudflare account.
type mailer struct {
	client    cloudflare.Client
	accountID string
	from      string
	hint      func(error) error // what to do about a token lacking permissions
}

func (m mailer) send(to, subject, text string) error {
	return m.hint(m.client.SendEmail(m.accountID, cloudflare.Email{
		FromAddress: m.from,
		FromName:    "Hakobu",
		To:          to,
		Subject:     subject,
		Text:        text + "\n\n-- \nhakobu at " + panelURL("") + "\nWhere these emails go: " + panelURL("/settings#notifications") + "\n",
	}))
}

// panelMailer sends from the panel's account, if its emails are on.
func panelMailer(s *store.Store) (mailer, error) {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return mailer{}, errNotifyOff
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return mailer{}, err
	}
	return mailer{c, cf.AccountID, n.SenderName + "@" + n.SenderDomain, tokenHint}, nil
}

// accountMailer sends from the account to to, if it's set up for that.
func accountMailer(s *store.Store, accountID int64, to string) (mailer, bool) {
	al, err := s.GetAccountAlerts(ctx(), accountID)
	if err != nil || !strings.EqualFold(al.Email, to) {
		return mailer{}, false
	}
	row, err := s.GetCloudflareAccount(ctx(), accountID)
	if err != nil {
		return mailer{}, false
	}
	a := clientAccount(s, row)
	return mailer{a.Client, a.AccountID, DefaultSenderName + "@" + al.SenderDomain, accountHint}, true
}

// userMailer sends to a user's address from one of their accounts set up
// for it, or else from the panel's.
func userMailer(s *store.Store, userID int64, to string) (mailer, error) {
	if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
		for _, r := range rows {
			if r.UserID != userID {
				continue
			}
			if m, ok := accountMailer(s, r.ID, to); ok {
				return m, nil
			}
		}
	}
	return panelMailer(s)
}

// mailerFor sends alert key to to: from the account of the project it's
// about, from one of its user's accounts if it's about no project, or
// from the panel's.
func mailerFor(s *store.Store, key, to string) (mailer, error) {
	about, ok := alertAbout(s, key)
	switch {
	case !ok:
	case about.account != 0:
		if m, ok := accountMailer(s, about.account, to); ok {
			return m, nil
		}
	case !about.project:
		return userMailer(s, about.user, to)
	}
	return panelMailer(s)
}

// AccountAlertsInfo is how a user's account alerts them, for Settings.
type AccountAlertsInfo struct {
	Title    string
	From     string // "" until set up
	Verified bool   // the address is confirmed there
	Watching int    // apps its watchdog checks
	History  bool   // the watchdog keeps uptime history
	Idle     bool   // no project there, so nothing's set up
	Err      string
}

// confirmedIn remembers addresses known to be confirmed in an account,
// by account ID and address.
var confirmedIn sync.Map

func userAccountAlerts(s *store.Store, userID int64) []AccountAlertsInfo {
	rows, err := s.ListCloudflareAccounts(ctx())
	if err != nil {
		return nil
	}
	var out []AccountAlertsInfo
	for _, r := range rows {
		if r.UserID != userID {
			continue
		}
		info := AccountAlertsInfo{Title: r.Title}
		if e, ok := accountAlertsErr.Load(r.ID); ok {
			info.Err = e.(string)
		}
		al, err := s.GetAccountAlerts(ctx(), r.ID)
		if err != nil {
			projects, _ := s.ProjectsInCloudflareAccount(ctx(), sql.NullInt64{Int64: r.ID, Valid: true})
			info.Idle = len(projects) == 0
			out = append(out, info)
			continue
		}
		info.From = DefaultSenderName + "@" + al.SenderDomain
		var targets []watchdogTarget
		_ = json.Unmarshal([]byte(al.Targets), &targets)
		info.Watching, info.History = len(targets), al.D1DatabaseID != ""
		key := fmt.Sprint(r.ID, " ", strings.ToLower(al.Email))
		if _, ok := confirmedIn.Load(key); ok {
			info.Verified = true
		} else if d, err := clientAccount(s, r).Client.Destination(r.AccountID, al.Email); err == nil {
			info.Verified = d.Verified
			if d.Verified {
				confirmedIn.Store(key, true)
			}
		} else if info.Err == "" {
			info.Err = accountHint(err).Error()
		}
		out = append(out, info)
	}
	return out
}

// accountUptimeDB is the D1 database of the watchdog that checks the app,
// if it's in a client's account (inClient): its client and ID, "" while
// it keeps no history.
func accountUptimeDB(s *store.Store, appID int64) (c cloudflare.Client, db string, inClient bool) {
	app, err := s.GetAppByID(ctx(), appID)
	if err != nil {
		return cloudflare.Client{}, "", false
	}
	a, err := appAccount(s, app)
	if err != nil || a.isPanel() {
		return cloudflare.Client{}, "", false
	}
	al, _ := s.GetAccountAlerts(ctx(), a.ID)
	return a.Client, al.D1DatabaseID, true
}
