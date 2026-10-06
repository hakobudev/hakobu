package ops

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// "Connect with Cloudflare": a user allows the panel's OAuth client
// (config.CloudflareClientID) into their account, the panel trades the
// code and keeps refreshing the token itself, and the account is connected
// as with an API token. Its token has no S3 keys, so its backups need an R2
// token as well (cfAccount.s3Client).

// CloudflareOAuth reports whether the panel offers "Connect with
// Cloudflare".
func CloudflareOAuth() bool { return config.CloudflareClientID != "" }

func cloudflareRedirect() string { return "https://" + config.PublicHost() + "/cloudflare/callback" }

// oauthLogins are sign-ins under way, by state, and sign-ins done whose
// account is still to pick, by a random ID; each for one user, for ten
// minutes.
var oauthLogins = struct {
	sync.Mutex
	started map[string]oauthStart
	done    map[string]oauthDone
}{started: map[string]oauthStart{}, done: map[string]oauthDone{}}

type oauthStart struct {
	user     int64
	verifier string
	at       time.Time
}

type oauthDone struct {
	user     int64
	token    cloudflare.Token
	accounts []cloudflare.Account
	at       time.Time
}

const oauthLoginTTL = 10 * time.Minute

func expireOAuthLogins(now time.Time) {
	for k, v := range oauthLogins.started {
		if now.Sub(v.at) > oauthLoginTTL {
			delete(oauthLogins.started, k)
		}
	}
	for k, v := range oauthLogins.done {
		if now.Sub(v.at) > oauthLoginTTL {
			delete(oauthLogins.done, k)
		}
	}
}

// StartCloudflareOAuth is where the user goes to allow the panel into
// their Cloudflare account.
func StartCloudflareOAuth(user int64) (string, error) {
	if !CloudflareOAuth() {
		return "", errors.New("this panel connects Cloudflare accounts with an API token")
	}
	state, err := RandomHex(16)
	if err != nil {
		return "", err
	}
	verifier, challenge := cloudflare.PKCE()
	oauthLogins.Lock()
	defer oauthLogins.Unlock()
	expireOAuthLogins(time.Now())
	oauthLogins.started[state] = oauthStart{user: user, verifier: verifier, at: time.Now()}
	return cloudflare.AuthorizeURL(config.CloudflareClientID, cloudflareRedirect(), state, challenge), nil
}

// FinishCloudflareOAuth trades the code Cloudflare sent back for tokens;
// the user then picks which of the accounts they reach to connect, under
// the ID returned (ConnectOAuthAccounts).
func FinishCloudflareOAuth(user int64, state, code string) (string, []cloudflare.Account, error) {
	oauthLogins.Lock()
	start, ok := oauthLogins.started[state]
	delete(oauthLogins.started, state)
	oauthLogins.Unlock()
	if !ok || start.user != user || time.Since(start.at) > oauthLoginTTL {
		return "", nil, errors.New("this sign-in expired or isn't yours: start it again from Settings → Cloudflare accounts")
	}
	t, err := cloudflare.Exchange(config.CloudflareClientID, cloudflareRedirect(), code, start.verifier)
	if err != nil {
		return "", nil, err
	}
	accounts, err := cloudflare.Client{Token: t.AccessToken}.Accounts()
	if err != nil {
		return "", nil, fmt.Errorf("listing the account's domains: %w", err)
	}
	if len(accounts) == 0 {
		return "", nil, errors.New("hakobu sees no domains in that Cloudflare account: add one there first")
	}
	id, err := RandomHex(16)
	if err != nil {
		return "", nil, err
	}
	oauthLogins.Lock()
	defer oauthLogins.Unlock()
	oauthLogins.done[id] = oauthDone{user: user, token: t, accounts: accounts, at: time.Now()}
	return id, accounts, nil
}

// OAuthChoice is an account a sign-in reached, with the name it would be
// connected as, or why it can't be.
type OAuthChoice struct {
	cloudflare.Account
	Name  string
	Taken string // "" if it can be connected
}

// OAuthChoices are the accounts to offer after a sign-in: each with a name
// of its own, and those that can't be connected (the panel's, or already
// connected) saying why.
func OAuthChoices(s *store.Store, user int64, accounts []cloudflare.Account) []OAuthChoice {
	panel, _ := panelAccount(s)
	names := map[string]bool{}
	out := make([]OAuthChoice, len(accounts))
	for i, a := range accounts {
		out[i].Account = a
		switch other, err := s.GetCloudflareAccountByAccountID(ctx(), a.ID); {
		case panel.AccountID == a.ID:
			out[i].Taken = "the panel's own account"
		case err == nil && other.UserID == user:
			out[i].Taken = "connected as " + other.Name
		case err == nil:
			out[i].Taken = "connected by someone else"
		default:
			out[i].Name = suggestAccountName(s, a.Name, names)
			names[out[i].Name] = true
		}
	}
	return out
}

// OAuthPick is an account to connect from a sign-in, and its name.
type OAuthPick struct{ Account, Name string }

// ConnectOAuthAccounts connects the accounts picked from the sign-in id,
// each under its name and with its tunnel, sharing the sign-in's tokens.
// Those that fail are told in the error; the others stay connected.
func ConnectOAuthAccounts(s *store.Store, user int64, id string, picks []OAuthPick) error {
	if len(picks) == 0 {
		return errors.New("pick at least one account")
	}
	oauthLogins.Lock()
	done, ok := oauthLogins.done[id]
	oauthLogins.Unlock()
	if !ok || done.user != user || time.Since(done.at) > oauthLoginTTL {
		return errors.New("this sign-in expired or isn't yours: start it again from Settings → Cloudflare accounts")
	}
	var failed []string
	for _, p := range picks {
		if err := connectOAuthAccount(s, user, id, done, p); err != nil {
			failed = append(failed, p.Name+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	oauthLogins.Lock()
	delete(oauthLogins.done, id)
	oauthLogins.Unlock()
	return nil
}

// connectOAuthAccount connects one account the sign-in reached; the
// sign-in's id groups the accounts that share its tokens.
func connectOAuthAccount(s *store.Store, user int64, signin string, done oauthDone, p OAuthPick) error {
	if err := checkName("client", p.Name); err != nil {
		return err
	}
	found := false
	for _, a := range done.accounts {
		found = found || a.ID == p.Account
	}
	if !found {
		return errors.New("that account isn't one this sign-in reaches")
	}
	t := done.token
	return connectAccount(s, user, p.Name, p.Account, cloudflare.Client{Token: t.AccessToken, AccountID: p.Account}, func() (int64, error) {
		return s.CreateOAuthCloudflareAccount(ctx(), store.CreateOAuthCloudflareAccountParams{
			Name: p.Name, ApiToken: secret.String(t.AccessToken), RefreshToken: secret.String(t.RefreshToken),
			TokenExpires: t.ExpiresAt.UTC().Format(time.RFC3339), Signin: signin, AccountID: p.Account, UserID: user,
		})
	})
}

// suggestAccountName makes a client name of a Cloudflare account's name,
// one neither connected nor in taken.
func suggestAccountName(s *store.Store, accountName string, taken map[string]bool) string {
	name := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(accountName), "-"), "-")
	name = strings.TrimSuffix(name, "-s-account")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "cf-" + name
	}
	if len(name) > 30 {
		name = strings.TrimRight(name[:30], "-")
	}
	base := name
	for i := 2; ; i++ {
		if _, err := s.GetCloudflareAccountByName(ctx(), name); err != nil && !taken[name] {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

var oauthRefresh sync.Mutex

// refreshBefore is how long before an OAuth token runs out it's renewed:
// time for many tries if Cloudflare's sign-in turns the server away for a
// while, as its bot protection may.
const refreshBefore = 30 * time.Minute

// freshToken is the account's token, for an OAuth one renewed first when
// it runs out within refreshBefore. The accounts of one sign-in share its
// tokens and are renewed together: a refresh token works only once. A
// renewal that fails leaves the old token, good till it runs out, and
// tells the accounts' owner.
func freshToken(s *store.Store, a store.CloudflareAccount) string {
	if a.RefreshToken == "" {
		return string(a.ApiToken)
	}
	oauthRefresh.Lock()
	defer oauthRefresh.Unlock()
	if cur, err := s.GetCloudflareAccount(ctx(), a.ID); err == nil {
		a = cur // another caller may have just renewed it
	}
	exp, err := time.Parse(time.RFC3339, a.TokenExpires)
	if err == nil && time.Until(exp) > refreshBefore {
		return string(a.ApiToken)
	}
	key, names := signInOf(s, a)
	t, err := cloudflare.Refresh(config.CloudflareClientID, string(a.RefreshToken))
	if err != nil {
		left := "it has run out"
		if until := time.Until(exp); until > 0 {
			left = fmt.Sprintf("the current one works for %s more", until.Round(time.Minute))
		}
		panellog.Error("renewing the Cloudflare sign-in of", names+":", err)
		problem(s, key, notifyAgain, "Cloudflare account "+names+" can't renew its sign-in",
			"hakobu couldn't renew its access to Cloudflare account "+names+"; "+left+". It keeps trying every few minutes.\n\n"+
				err.Error()+"\n\nIf it keeps failing, connect the account with an API token instead, which never needs renewing: Settings → Cloudflare accounts → "+a.Name+" → Replace token.\n\n"+panelURL("/settings#clients"))
		return string(a.ApiToken)
	}
	tokens := secret.String(t.AccessToken)
	refresh := secret.String(t.RefreshToken)
	expires := t.ExpiresAt.UTC().Format(time.RFC3339)
	if a.Signin != "" {
		err = s.SetCloudflareSignIn(ctx(), store.SetCloudflareSignInParams{ApiToken: tokens, RefreshToken: refresh, TokenExpires: expires, Signin: a.Signin})
	} else {
		err = s.SetCloudflareAccountOAuth(ctx(), store.SetCloudflareAccountOAuthParams{ApiToken: tokens, RefreshToken: refresh, TokenExpires: expires, ID: a.ID})
	}
	if err != nil {
		panellog.Error("saving the renewed Cloudflare sign-in of", names+":", err)
	}
	solved(s, key, "Cloudflare account "+names+" renews its sign-in again", "hakobu renewed its access to Cloudflare account "+names+".")
	return t.AccessToken
}

// signInOf is the problem key of the account's sign-in and the names of
// the accounts sharing it, for telling the owner once for all of them.
func signInOf(s *store.Store, a store.CloudflareAccount) (key, names string) {
	if a.Signin == "" {
		return "cloudflare:" + a.Name, a.Name
	}
	var all []string
	if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
		for _, r := range rows {
			if r.Signin == a.Signin {
				all = append(all, r.Name)
			}
		}
	}
	if len(all) == 0 {
		all = []string{a.Name}
	}
	return "cloudflare-signin:" + a.Signin, strings.Join(all, ", ")
}

// RenewCloudflareSignIns keeps the OAuth accounts' tokens fresh, every
// interval, whether they're used or not, so a renewal that fails is
// retried long before the token runs out; once per sign-in.
func RenewCloudflareSignIns(s *store.Store, interval time.Duration) {
	for {
		if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
			seen := map[string]bool{}
			for _, a := range rows {
				if a.RefreshToken == "" || (a.Signin != "" && seen[a.Signin]) {
					continue
				}
				seen[a.Signin] = true
				freshToken(s, a)
			}
		}
		time.Sleep(interval)
	}
}

// SetR2Token gives an OAuth account the API token its backups' S3 keys
// come from; it must be for that account and reach its R2.
func SetR2Token(s *store.Store, name, token string) error {
	row, err := s.GetCloudflareAccountByName(ctx(), name)
	if err != nil {
		return fmt.Errorf("Cloudflare account %q not found", name)
	}
	token = strings.TrimSpace(token)
	c := cloudflare.Client{Token: token, AccountID: row.AccountID}
	if _, _, err := c.R2Credentials(); err != nil {
		return fmt.Errorf("the token doesn't work for account %s: make it in that account, as an account-owned token (%w)", name, err)
	}
	if _, err := c.R2On(row.AccountID); err != nil {
		return fmt.Errorf("the token can't reach R2: give it Workers R2 Storage Edit (%w)", err)
	}
	return s.SetCloudflareAccountR2Token(ctx(), store.SetCloudflareAccountR2TokenParams{R2Token: secret.String(token), ID: row.ID})
}

// The relay: a Worker of the panel's own, in its own Cloudflare account,
// that takes the panel's token requests to Cloudflare's token endpoint from
// inside Cloudflare, which its bot protection doesn't challenge. Only for a
// panel that offers "Connect with Cloudflare", where the panel holds those
// tokens anyway: the Worker is the panel's, not a third party's. Requests go
// to it only after the endpoint challenged the server (cloudflare.SetTokenRelay).

//go:embed token_relay.js
var tokenRelayJS string

const tokenRelayCompatibilityDate = "2026-09-01"

// tokenRelayName is the relay Worker's name, one per panel.
func tokenRelayName() string {
	sum := sha256.Sum256([]byte(config.PublicHost()))
	return "hakobu-token-relay-" + hex.EncodeToString(sum[:4])
}

// EnsureTokenRelay deploys the relay Worker with a new secret and points
// the panel's token requests' fallback at it. Run when the panel starts; a
// panel without the relay asks the endpoint directly, as before.
func EnsureTokenRelay(s *store.Store) error {
	if !CloudflareOAuth() {
		return nil
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	sub, err := c.WorkersSubdomain(cf.AccountID)
	if err != nil {
		return workersHint(err)
	}
	secret, err := RandomHex(32)
	if err != nil {
		return err
	}
	name := tokenRelayName()
	bindings := []cloudflare.Binding{
		{"type": "secret_text", "name": "SECRET", "text": secret},
		{"type": "plain_text", "name": "CLIENT_ID", "text": config.CloudflareClientID},
	}
	if err := c.UploadWorker(cf.AccountID, name, tokenRelayJS, tokenRelayCompatibilityDate, bindings); err != nil {
		return workersHint(err)
	}
	if err := c.EnableWorkersDev(cf.AccountID, name); err != nil {
		return workersHint(err)
	}
	cloudflare.SetTokenRelay("https://"+name+"."+sub+".workers.dev", secret)
	return nil
}
