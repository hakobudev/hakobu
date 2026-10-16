package ops

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// The owner gets an email when something needs them: a deploy after a push
// failed, a backup failed, an app crashed or ran out of memory, the disk
// is filling up. It goes through Cloudflare Email Service to an address the owner
// confirmed, which is free, from alerts@<panel host> (the name before the @
// can be changed). Cloudflare sends
// from any name in a zone with Email Routing enabled; where it isn't,
// hakobu enables it by turning it on for mail.<panel host>, which leaves
// the mail of the domain itself alone.
//
// Alerts about a project in a client's Cloudflare account come from that
// account instead (account_alerts.go).
//
// A problem is mailed once, then again only if it's still there hours
// later; when it's gone, one more email says so. What was mailed is kept
// in memory, so a restart may repeat an email.

// DefaultSenderName comes before the @ of the sender unless the owner
// picks another: the panel's host is hakobu.<domain> on most panels, and
// hakobu@hakobu.… reads twice.
const DefaultSenderName = "alerts"

var senderName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// notifyAgain is how long a problem that's still there stays quiet.
const notifyAgain = 6 * time.Hour

var problems struct {
	sync.Mutex
	mailed map[string]time.Time // by problem key, when last mailed
}

// errNotifyOff: nobody to mail.
var errNotifyOff = errors.New("notifications are off")

// NotifyInfo describes the notifications for Settings.
type NotifyInfo struct {
	On       bool
	Email    string
	Name     string // before the @ of From
	From     string
	Verified bool   // the owner confirmed the address
	Err      string // why it couldn't be checked
}

// confirmed remembers addresses known to be confirmed, so Settings asks
// Cloudflare only until the owner clicks the link.
var confirmed sync.Map

// Notifications reports where emails go and whether the address is
// confirmed.
func Notifications(s *store.Store) NotifyInfo {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return NotifyInfo{}
	}
	info := NotifyInfo{On: true, Email: n.Email, Name: n.SenderName, From: n.SenderName + "@" + n.SenderDomain}
	if _, ok := confirmed.Load(n.Email); ok {
		info.Verified = true
		return info
	}
	c, cf, err := cfClient(s)
	if err == nil {
		var d cloudflare.Destination
		d, err = c.Destination(cf.AccountID, n.Email)
		info.Verified = d.Verified
	}
	if err != nil {
		info.Err = tokenHint(err).Error()
	} else if info.Verified {
		confirmed.Store(n.Email, true)
	}
	return info
}

// NotifyChoice is an address or a domain to offer in Settings.
type NotifyChoice struct {
	Value, Note string
	Selected    bool
}

// NotifyChoices are the addresses to send to that need no confirming or
// are the owner's on GitHub, the domains that can send, and why the other
// domains of the account can't.
func NotifyChoices(s *store.Store) (to, from []NotifyChoice, why []string, err error) {
	c, cf, err := cfClient(s)
	if err != nil {
		return nil, nil, nil, err
	}
	current, _ := s.GetNotify(ctx())
	dests, err := c.Destinations(cf.AccountID)
	if err != nil {
		return nil, nil, nil, tokenHint(err)
	}
	github := ""
	if o, err := s.Admin(ctx()); err == nil {
		github = o.GitHubEmail
	}
	for _, d := range dests {
		if !d.Verified {
			continue
		}
		note := "confirmed in Cloudflare"
		if strings.EqualFold(d.Email, github) {
			note, github = "your GitHub email, confirmed in Cloudflare", ""
		}
		to = append(to, NotifyChoice{Value: d.Email, Note: note, Selected: strings.EqualFold(d.Email, current.Email)})
	}
	if github != "" {
		to = append(to, NotifyChoice{Value: github, Note: "your GitHub email, Cloudflare asks to confirm it", Selected: strings.EqualFold(github, current.Email)})
	}

	zones, err := c.Zones()
	if err != nil {
		return nil, nil, nil, err
	}
	host := config.PublicHost()
	if zone, ok := cloudflare.ZoneFor(zones, host); ok {
		r, err := c.EmailRouting(zone.ID)
		if err != nil {
			return nil, nil, nil, tokenHint(err)
		}
		note := "Email Routing is on for " + zone.Name
		if !r.Enabled {
			note = "hakobu turns Email Routing on for " + zone.Name + " through mail." + host + "; the domain's own mail isn't touched"
		}
		from = append(from, NotifyChoice{Value: host, Note: note})
		zones = append([]cloudflare.Zone{zone}, zones...) // the panel's first
	}
	seen := map[string]bool{host: true}
	for _, z := range zones {
		if seen[z.Name] {
			continue
		}
		seen[z.Name] = true
		r, err := c.EmailRouting(z.ID)
		if err != nil {
			return nil, nil, nil, tokenHint(err)
		}
		switch apex, err := apexChoice(c, z, r); {
		case err != nil:
			return nil, nil, nil, tokenHint(err)
		case apex.Value != "":
			from = append(from, apex)
		default:
			why = append(why, apex.Note)
		}
	}
	for i := range from {
		from[i].Selected = from[i].Value == current.SenderDomain
	}
	return to, from, why, nil
}

// apexChoice offers a zone's apex to send from if Email Routing works
// there, or can be turned on because the domain gets no mail; otherwise
// its Note says why not.
func apexChoice(c cloudflare.Client, z cloudflare.Zone, r cloudflare.Routing) (NotifyChoice, error) {
	if r.ApexReady {
		return NotifyChoice{Value: z.Name, Note: "Email Routing is on"}, nil
	}
	mx, err := c.MXRecords(z.ID, z.Name)
	if err != nil {
		return NotifyChoice{}, err
	}
	if len(mx) == 0 {
		return NotifyChoice{Value: z.Name, Note: "hakobu turns Email Routing on: the domain gets no mail now"}, nil
	}
	return NotifyChoice{Note: fmt.Sprintf("%s: its mail goes to %s, and turning Email Routing on would take it over", z.Name, mx[0])}, nil
}

// SetupNotifications sends emails to email from now on, from the domain
// from if given (one of the account's), and asks Cloudflare to have the
// address confirmed. What hakobu set up for an earlier address or domain
// and no longer needs is removed.
func SetupNotifications(s *store.Store, email, name, from string) error {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || addr.Name != "" {
		return fmt.Errorf("%q isn't an email address", email)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = DefaultSenderName
	}
	if !senderName.MatchString(name) {
		return fmt.Errorf("%q can't come before the @: use letters, digits, dots, dashes and underscores", name)
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	if cf.AccountID == "" {
		return fmt.Errorf("finish `hakobu setup` first")
	}
	zones, err := c.Zones()
	if err != nil {
		return err
	}
	sender, zone, routed, err := senderDomain(c, zones, strings.Trim(strings.ToLower(strings.TrimSpace(from)), "."))
	if err != nil {
		return tokenHint(err)
	}
	added, err := c.AddDestination(cf.AccountID, addr.Address)
	if err != nil {
		return tokenHint(err)
	}
	n := store.SaveNotifyParams{Email: addr.Address, SenderName: name, SenderDomain: sender, ZoneID: zone.ID, RoutedDomain: routed}
	prev, prevErr := s.GetNotify(ctx())
	hadPrev := prevErr == nil
	if hadPrev { // what hakobu set up before and still uses stays its to undo
		added = added || (prev.AddedAddress == 1 && strings.EqualFold(prev.Email, addr.Address))
		if prev.RoutedDomain != "" && prev.ZoneID == zone.ID && n.RoutedDomain == "" {
			n.RoutedDomain = prev.RoutedDomain // it keeps the zone's Email Routing on
		}
	}
	if added {
		n.AddedAddress = 1
	}
	if err := s.SaveNotify(ctx(), n); err != nil {
		return err
	}
	if hadPrev {
		err = undoNotify(c, cf.AccountID, prev, addr.Address, n.RoutedDomain)
	}
	if werr := EnsureWatchdog(s, true); werr != nil {
		err = errors.Join(err, fmt.Errorf("the watchdog isn't set up: %w", werr))
	}
	async(func() { EnsureAccountAlerts(s) }) // admins' alerts go to the address
	return err
}

// senderDomain picks the domain to send from, the panel's host if from is
// "", and makes sure its zone has Email Routing on. It returns the domain
// with its zone and the subdomain hakobu turned Email Routing on for, if
// it did. It turns it on for a zone's apex only if the domain has no MX
// records: otherwise that would take over its mail.
func senderDomain(c cloudflare.Client, zones []cloudflare.Zone, from string) (sender string, zone cloudflare.Zone, routed string, err error) {
	sender = from
	if sender == "" {
		sender = config.PublicHost()
	}
	zone, ok := cloudflare.ZoneFor(zones, sender)
	if !ok {
		return "", zone, "", fmt.Errorf("%s is not in a domain of your Cloudflare account", sender)
	}
	r, err := c.EmailRouting(zone.ID)
	if err != nil {
		return "", zone, "", err
	}
	switch {
	case sender == zone.Name && r.ApexReady:
		return sender, zone, "", nil
	case sender == zone.Name:
		// Left on when emails are turned off: it can only be turned off
		// for the whole zone, which would take any routing the owner adds.
		apex, err := apexChoice(c, zone, r)
		if err == nil && apex.Value == "" {
			err = fmt.Errorf("can't send from %s; send from a subdomain", apex.Note)
		}
		if err == nil {
			err = enableRouting(c, zone, zone.Name)
		}
		return sender, zone, "", err
	case r.Enabled:
		return sender, zone, "", nil
	}
	routed = "mail." + sender
	if err := enableRouting(c, zone, routed); err != nil {
		return "", zone, "", err
	}
	return sender, zone, routed, nil
}

// enableRouting turns Email Routing on for name. For a subdomain,
// Cloudflare also writes an SPF record at the apex that allows only its
// own servers, which would fail the domain's own mail sent elsewhere;
// that's put back as it was.
func enableRouting(c cloudflare.Client, zone cloudflare.Zone, name string) error {
	if name == zone.Name { // the apex sends: its SPF must allow Cloudflare
		return c.EnableEmailRouting(zone.ID, "")
	}
	before, err := c.SPFRecords(zone.ID, zone.Name)
	if err != nil {
		return err
	}
	if err := c.EnableEmailRouting(zone.ID, name); err != nil {
		return err
	}
	after, err := c.SPFRecords(zone.ID, zone.Name)
	if err != nil {
		return err
	}
	for id, content := range after {
		was, existed := before[id]
		switch {
		case !existed:
			err = c.DeleteRecord(zone.ID, id)
		case was != content:
			err = c.SetTXT(zone.ID, id, was)
		}
		if err != nil {
			return fmt.Errorf("putting back the SPF record of %s: %w", zone.Name, err)
		}
	}
	return nil
}

// undoNotify removes from Cloudflare what hakobu set up for n, except the
// address and domain still in use.
func undoNotify(c cloudflare.Client, accountID string, n store.Notify, keepEmail, keepDomain string) error {
	var errs []error
	if n.AddedAddress == 1 && !strings.EqualFold(n.Email, keepEmail) {
		if err := c.DeleteDestination(accountID, n.Email); err != nil {
			errs = append(errs, fmt.Errorf("removing %s from Cloudflare's addresses: %w", n.Email, err))
		}
		confirmed.Delete(n.Email)
	}
	if n.RoutedDomain != "" && n.RoutedDomain != keepDomain {
		err := c.DisableEmailRouting(n.ZoneID, n.RoutedDomain)
		if err == nil {
			var spf map[string]string
			if spf, err = c.SPFRecords(n.ZoneID, n.RoutedDomain); err == nil {
				for id, content := range spf {
					if content != cloudflare.CloudflareSPF {
						continue // the owner's own
					}
					if err = c.DeleteRecord(n.ZoneID, id); err != nil {
						break
					}
				}
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("turning Email Routing off for %s: %w", n.RoutedDomain, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return tokenHint(err)
	}
	return nil
}

// tokenHint names the permissions email needs when Cloudflare refused a
// valid token: one made before hakobu sent email lacks them.
func tokenHint(err error) error {
	if err == nil || !strings.Contains(err.Error(), cloudflare.TokenLacksPermission) {
		return err
	}
	return fmt.Errorf("%w; email needs Zone Settings Edit, Email Routing Addresses Edit and Email Sending Edit: give hakobu a new token with `cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect`", err)
}

// TurnOffNotifications stops the emails and removes from Cloudflare what
// hakobu set up for them: the watchdog, the address, if hakobu added it,
// and Email Routing for the domain hakobu turned it on for.
func TurnOffNotifications(s *store.Store) error {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return nil
	}
	// The watchdog mails the same address: it goes first, or it would
	// keep mailing with nothing left in hakobu to turn it off.
	if err := DisableWatchdog(s); err != nil {
		return fmt.Errorf("emails stay on: removing the watchdog failed: %w", err)
	}
	if err := s.DeleteNotify(ctx()); err != nil {
		return err
	}
	c, cf, err := cfClient(s)
	if err == nil {
		err = undoNotify(c, cf.AccountID, n, "", "")
	}
	if err != nil {
		return fmt.Errorf("emails are off, but cleaning up in Cloudflare failed (remove it there by hand): %w", err)
	}
	return nil
}

// SendTestEmail sends an email right away.
func SendTestEmail(s *store.Store) error {
	return mailOwner(s, "Test email", "Notifications from your hakobu panel arrive here.")
}

// mailOwner sends an email to the panel's address, if notifications are
// on.
func mailOwner(s *store.Store, subject, text string) error {
	n, err := s.GetNotify(ctx())
	if err != nil {
		return errNotifyOff
	}
	return mailTo(s, n.Email, subject, text)
}

// mailTo sends an email from the panel's sender, if notifications are on.
func mailTo(s *store.Store, to, subject, text string) error {
	m, err := panelMailer(s)
	if err != nil {
		return err
	}
	return m.send(to, subject, text)
}

// problem mails whoever it's for about the problem key unless it was
// mailed within again.
func problem(s *store.Store, key string, again time.Duration, subject, text string) {
	if recipient(s, key) == "" {
		return
	}
	problems.Lock()
	if problems.mailed == nil {
		problems.mailed = map[string]time.Time{}
	}
	last, ok := problems.mailed[key]
	if ok && time.Since(last) < again {
		problems.Unlock()
		return
	}
	problems.mailed[key] = time.Now()
	problems.Unlock()
	async(func() { sendFor(s, key, subject, text) })
}

// solved mails the owner that the problem key is gone, if it was mailed;
// with subject "" it only forgets it.
func solved(s *store.Store, key, subject, text string) {
	problems.Lock()
	_, ok := problems.mailed[key]
	delete(problems.mailed, key)
	problems.Unlock()
	if ok && subject != "" {
		async(func() { sendFor(s, key, subject, text) })
	}
}

// sendFor mails the alert of problem key to whoever it's for.
func sendFor(s *store.Store, key, subject, text string) {
	to := recipient(s, key)
	if to == "" {
		return
	}
	m, err := mailerFor(s, key, to)
	if err == nil {
		err = m.send(to, subject, text)
	}
	if err != nil && !errors.Is(err, errNotifyOff) {
		panellog.Error("failed to email", to+":", err)
	}
}

// recipient is where the alert of problem key goes: to the user whose
// project or server it's about, at their own address, or at the panel's if
// they're an admin without one; the server's own alerts go to the panel's
// address. "" for nobody.
func recipient(s *store.Store, key string) string {
	about, ok := alertAbout(s, key)
	if ok {
		if u, err := s.GetUser(ctx(), about.user); err == nil {
			return userAddress(s, u)
		}
	}
	// The server's own, or nobody's, as made before there were users.
	if n, err := s.GetNotify(ctx()); err == nil {
		return n.Email
	}
	return ""
}

// alertSubject is whose an alert is: the user's, about a project of theirs
// (in their client's Cloudflare account, hakobu's ID, or 0 for the
// panel's), or about something else of theirs.
type alertSubject struct {
	user    int64
	project bool
	account int64
}

// alertAbout is whose problem key is: an app's project (deploy, crash, out
// of memory, health, memory use, by the app's ID), a database's or a
// volume's (backup, by dbJobKey or volumeJob), a database's or cache's that
// doesn't answer (by its ID), a server's (its key not
// exported, it's away) or a Cloudflare account's. ok is false for the
// panel's own alerts.
func alertAbout(s *store.Store, key string) (alertSubject, bool) {
	kind, rest, _ := strings.Cut(key, ":")
	var projectID int64
	switch kind {
	case "deploy", "oom", "crash", "health", "memory":
		if kind == "memory" { // memory:app:<id> or memory:worker:<id>
			var ok bool
			if _, rest, ok = strings.Cut(rest, ":"); !ok {
				return alertSubject{}, false
			}
		}
		id, _ := strconv.ParseInt(rest, 10, 64)
		a, err := s.GetAppByID(ctx(), id)
		if err != nil {
			return alertSubject{}, false
		}
		projectID = a.ProjectID
	case "backup":
		app, db, ok := backupOf(s, rest)
		switch {
		case !ok:
			return alertSubject{}, false // the panel's own backup
		case app != nil:
			projectID = app.ProjectID
		default:
			projectID = db.ProjectID
		}
	case "dbdown": // dbdown:<database ID>, not answering
		id, _ := strconv.ParseInt(rest, 10, 64)
		d, err := s.GetDatabaseByID(ctx(), id)
		if err != nil {
			return alertSubject{}, false
		}
		projectID = d.ProjectID
	case "cachedown": // cachedown:<cache ID>
		id, _ := strconv.ParseInt(rest, 10, 64)
		c, err := s.GetCacheByID(ctx(), id)
		if err != nil {
			return alertSubject{}, false
		}
		projectID = c.ProjectID
	case "serverkey", "server", "servermemory", "servercpu", "serverdisk", "serverdiskfilling", "serverdiskfast": // <kind>:<server ID>
		id, _ := strconv.ParseInt(rest, 10, 64)
		n, err := s.GetNode(ctx(), id)
		if err != nil {
			return alertSubject{}, false
		}
		return alertSubject{user: n.UserID}, true
	case "cloudflare": // cloudflare:<account>, a sign-in that can't renew
		a, err := s.GetCloudflareAccountByName(ctx(), rest)
		if err != nil {
			return alertSubject{}, false
		}
		return alertSubject{user: a.UserID}, true
	default:
		return alertSubject{}, false
	}
	p, err := s.GetProjectByID(ctx(), projectID)
	if err != nil {
		return alertSubject{}, false
	}
	return alertSubject{user: p.UserID, project: true, account: p.CloudflareAccountID.Int64}, true
}

// backupOf is what a backup job (dbJobKey, volumeJob) is of: the app whose
// volume it is, or the database.
func backupOf(s *store.Store, job string) (app *store.App, db *store.Database, ok bool) {
	kind, rest, _ := strings.Cut(job, ":")
	switch kind {
	case "db":
		id, _ := strconv.ParseInt(rest, 10, 64)
		if d, err := s.GetDatabaseByID(ctx(), id); err == nil {
			return nil, &d, true
		}
	case "vol":
		appID, _, _ := strings.Cut(rest, "/")
		id, _ := strconv.ParseInt(appID, 10, 64)
		if a, err := s.GetAppByID(ctx(), id); err == nil {
			return &a, nil, true
		}
	}
	return nil, nil, false
}

// AppPath is the app's page in the panel.
func AppPath(app store.App) string {
	return "/projects/" + url.PathEscape(app.ProjectName) + "/apps/" + url.PathEscape(app.Name)
}

// CachePath is the cache's page in the panel.
func CachePath(s *store.Store, c store.Cache) string {
	p, err := s.GetProjectByID(ctx(), c.ProjectID)
	if err != nil {
		return "/"
	}
	return "/projects/" + url.PathEscape(p.Name) + "/caches/" + url.PathEscape(c.Name)
}

// DatabasePath is the database's page in the panel.
func DatabasePath(s *store.Store, d store.Database) string {
	p, err := s.GetProjectByID(ctx(), d.ProjectID)
	if err != nil {
		return "/"
	}
	return "/projects/" + url.PathEscape(p.Name) + "/databases/" + url.PathEscape(d.Name)
}

// async runs an email's sending off the caller's path; tests wait for it.
var async = func(f func()) { go f() }

func sendOrLog(s *store.Store, subject, text string) {
	if err := mailOwner(s, subject, text); err != nil && !errors.Is(err, errNotifyOff) {
		panellog.Error("failed to email the owner:", err)
	}
}

func panelURL(path string) string { return "https://" + config.PublicHost() + path }

// noteDeploy mails a deploy that failed after a push, when nobody was
// watching, and the next one that works.
func noteDeploy(s *store.Store, app store.App, trigger string, err error) {
	key := fmt.Sprintf("deploy:%d", app.ID)
	if err == nil {
		solved(s, key, app.Name+": deploys work again", "The latest deploy of "+app.Name+" succeeded.\n\n"+panelURL(AppPath(app)))
		return
	}
	if trigger != "push" {
		return
	}
	problem(s, key, notifyAgain, app.Name+": deploy failed",
		fmt.Sprintf("Deploying %s after a push to GitHub failed:\n\n  %v\n\nThe deploy log: %s", app.Name, err, panelURL(AppPath(app)+"/deployments")))
}

// NoteBackup mails a failed backup of a database or a volume (job: its
// dbJobKey or volumeJob), or of the panel with job "panel", and the next
// one that works.
func NoteBackup(s *store.Store, job string, err error) {
	what, path := "the panel", "/settings"
	if app, db, ok := backupOf(s, job); ok && app != nil {
		_, volume, _ := strings.Cut(job, "/")
		what, path = "volume "+volume+" of "+app.Name, AppPath(*app)
	} else if ok {
		what, path = "database "+db.Name, DatabasePath(s, *db)
	} else if job != "panel" {
		return // gone since
	}
	key := "backup:" + job
	if err == nil {
		solved(s, key, "Backups of "+what+" work again", "The latest backup of "+what+" succeeded.\n\n"+panelURL(path))
		return
	}
	problem(s, key, notifyAgain, "Backup of "+what+" failed",
		fmt.Sprintf("Backing up %s failed:\n\n  %v\n\nhakobu tries again within the hour. %s", what, err, panelURL(path)))
}

// noteOOM mails an app or worker killed for running out of memory.
func noteOOM(s *store.Store, app store.App, message string) {
	problem(s, fmt.Sprintf("oom:%d", app.ID), notifyAgain, app.Name+": out of memory",
		message+"\n\nGive it more memory or find what grows: "+panelURL(AppPath(app)+"/errors"))
}

// noteCrash mails a live app or worker container that exited with an
// error; Docker restarting it again and again stays one email.
func noteCrash(s *store.Store, app store.App, message string) {
	problem(s, fmt.Sprintf("crash:%d", app.ID), notifyAgain, app.Name+": crashed",
		message+"\n\nIts output: "+panelURL(AppPath(app)+"/logs"))
}

// CheckForOwner mails a master key the owner
// hasn't downloaded, checks the token and puts the watchdog back if it's
// gone; it's called hourly.
func CheckForOwner(s *store.Store) {
	TokenPermissions(s, true)
	if err := EnsureWatchdog(s, false); err != nil {
		panellog.Error("watchdog:", err)
	}
	EnsureAccountAlerts(s)
	checkServerKeys(s)
	if BackupBucket(s) != "" && panelBackedUp() && !KeyDownloaded() {
		problem(s, "key", 7*24*time.Hour, "Download the panel's master key",
			"The panel is backed up, but you have no copy of its current master key, and without it the backups can't be restored. Download it and keep it away from the server: "+panelURL("/settings/master-key"))
	} else {
		solved(s, "key", "", "")
	}
}

// NoteOAuthConnection mails the user that an app was given access in their
// name through OAuth, so a connection they didn't make doesn't go
// unnoticed.
func NoteOAuthConnection(s *store.Store, u store.User, client, redirectHost string, scopes []string) {
	to := userAddress(s, u)
	if to == "" {
		return
	}
	async(func() {
		m, err := userMailer(s, u.ID, to)
		if err != nil {
			return
		}
		err = m.send(to, client+" connected to hakobu", fmt.Sprintf(
			"You allowed %s (it returns to %s) to use hakobu with scopes: %s.\n\nIf that wasn't you, disconnect it and sign out everywhere: %s",
			client, redirectHost, strings.Join(scopes, ", "), panelURL("/settings#ai-apps")))
		if err != nil && !errors.Is(err, errNotifyOff) {
			panellog.Error("failed to email", to+":", err)
		}
	})
}

// UserNotify is a user's own alerts address as Settings shows it.
type UserNotify struct {
	PanelOn bool   // the panel's account sends emails
	Email   string // '' if none
	// Alerts about their projects in the panel's account come from there:
	// they have some, and whether the address is confirmed there.
	InPanel  bool
	Verified bool
	Err      string
	Accounts []AccountAlertsInfo // their clients' accounts, which send the rest
}

// CanSend: something can send the user's alerts.
func (n UserNotify) CanSend() bool { return n.PanelOn || len(n.Accounts) > 0 }

func UserNotifyOf(s *store.Store, u store.User) UserNotify {
	v := UserNotify{Email: u.NotifyEmail, Accounts: userAccountAlerts(s, u.ID)}
	if _, err := s.GetNotify(ctx()); err != nil {
		return v
	}
	v.PanelOn = true
	projects, _ := s.UserProjectsInPanelAccount(ctx(), u.ID)
	v.InPanel = len(projects) > 0 && !config.PanelOnly
	if v.Email == "" || !v.InPanel {
		return v
	}
	c, cf, err := cfClient(s)
	if err == nil {
		var d cloudflare.Destination
		if d, err = c.Destination(cf.AccountID, v.Email); err == nil {
			v.Verified = d.Verified
		}
	}
	if err != nil {
		v.Err = tokenHint(err).Error()
	}
	return v
}

// SetUserNotify sets where a user's own alerts go, or with email "" stops
// them (an admin's go to the panel's address then). Each Cloudflare
// account of theirs with projects sends the alerts about those, to the
// address confirmed there, and the panel's account the rest: Cloudflare
// mails the user a link to confirm it in each. A panel-only panel's
// account confirms no users' addresses: it would run out.
func SetUserNotify(s *store.Store, u store.User, email string) error {
	email = strings.TrimSpace(email)
	if email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Name != "" {
			return fmt.Errorf("%q isn't an email address", email)
		}
		email = addr.Address
		_, panelErr := s.GetNotify(ctx())
		n, _ := s.CountCloudflareAccountsOf(ctx(), u.ID)
		if panelErr != nil && n == 0 {
			return errors.New("nothing can send your alerts yet: connect your Cloudflare account under Cloudflare, or ask the panel's admin to turn emails on")
		}
		if panelErr == nil && !config.PanelOnly {
			c, cf, err := cfClient(s)
			if err != nil {
				return err
			}
			if _, err := c.AddDestination(cf.AccountID, email); err != nil {
				return tokenHint(err)
			}
		}
	}
	if err := s.SetUserNotifyEmail(ctx(), store.SetUserNotifyEmailParams{NotifyEmail: email, ID: u.ID}); err != nil {
		return err
	}
	if err := ensureUserAccountAlerts(s, u.ID); err != nil {
		return fmt.Errorf("saved, but alerts from your Cloudflare accounts aren't set up: %w", err)
	}
	return nil
}

// SendUserTestEmail sends a test email to a user's own address from each
// account that sends their alerts.
func SendUserTestEmail(s *store.Store, u store.User) error {
	if u.NotifyEmail == "" {
		return errors.New("set your address first")
	}
	const subject, text = "Test email", "Alerts about your projects on this hakobu panel arrive here."
	var errs []error
	sent := 0
	if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
		for _, r := range rows {
			if r.UserID != u.ID {
				continue
			}
			if m, ok := accountMailer(s, r.ID, u.NotifyEmail); ok {
				sent++
				if err := m.send(u.NotifyEmail, subject, text+"\nThis one came from Cloudflare account "+r.Title+"."); err != nil {
					errs = append(errs, fmt.Errorf("from %s: %w", r.Title, err))
				}
			}
		}
	}
	if projects, _ := s.UserProjectsInPanelAccount(ctx(), u.ID); sent == 0 || (len(projects) > 0 && !config.PanelOnly) {
		if err := mailTo(s, u.NotifyEmail, subject, text); err != nil {
			if errors.Is(err, errNotifyOff) && sent == 0 {
				err = errors.New("nothing sends your alerts yet: they're set up once a project of yours is in a Cloudflare account of yours")
			}
			if !errors.Is(err, errNotifyOff) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
