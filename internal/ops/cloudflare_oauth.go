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
// the user then picks one of the accounts they reach, under the ID
// returned (ConnectOAuthAccount).
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

// ConnectOAuthAccount connects account, one the sign-in id reached, as
// name, and creates its tunnel.
func ConnectOAuthAccount(s *store.Store, user int64, id, account, name string) error {
	if err := checkName("client", name); err != nil {
		return err
	}
	oauthLogins.Lock()
	done, ok := oauthLogins.done[id]
	oauthLogins.Unlock()
	if !ok || done.user != user || time.Since(done.at) > oauthLoginTTL {
		return errors.New("this sign-in expired or isn't yours: start it again from Settings → Cloudflare accounts")
	}
	found := false
	for _, a := range done.accounts {
		found = found || a.ID == account
	}
	if !found {
		return errors.New("that account isn't one this sign-in reaches")
	}
	t := done.token
	err := connectAccount(s, user, name, account, cloudflare.Client{Token: t.AccessToken, AccountID: account}, func() (int64, error) {
		return s.CreateOAuthCloudflareAccount(ctx(), store.CreateOAuthCloudflareAccountParams{
			Name: name, ApiToken: secret.String(t.AccessToken), RefreshToken: secret.String(t.RefreshToken),
			TokenExpires: t.ExpiresAt.UTC().Format(time.RFC3339), AccountID: account, UserID: user,
		})
	})
	if err == nil {
		oauthLogins.Lock()
		delete(oauthLogins.done, id)
		oauthLogins.Unlock()
	}
	return err
}

// SuggestAccountName makes a client name of a Cloudflare account's name.
func SuggestAccountName(s *store.Store, accountName string) string {
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
		if _, err := s.GetCloudflareAccountByName(ctx(), name); err != nil {
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
// it runs out within refreshBefore. A renewal that fails leaves the old
// token, good till it runs out, and tells the account's owner.
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
	t, err := cloudflare.Refresh(config.CloudflareClientID, string(a.RefreshToken))
	if err != nil {
		left := "it has run out"
		if until := time.Until(exp); until > 0 {
			left = fmt.Sprintf("the current one works for %s more", until.Round(time.Minute))
		}
		panellog.Error("renewing the Cloudflare sign-in of", a.Name+":", err)
		problem(s, "cloudflare:"+a.Name, notifyAgain, "Cloudflare account "+a.Name+" can't renew its sign-in",
			"hakobu couldn't renew its access to Cloudflare account "+a.Name+"; "+left+". It keeps trying every few minutes.\n\n"+
				err.Error()+"\n\nIf it keeps failing, connect the account with an API token instead, which never needs renewing: Settings → Cloudflare accounts → "+a.Name+" → Replace token.\n\n"+panelURL("/settings#clients"))
		return string(a.ApiToken)
	}
	if err := s.SetCloudflareAccountOAuth(ctx(), store.SetCloudflareAccountOAuthParams{
		ApiToken: secret.String(t.AccessToken), RefreshToken: secret.String(t.RefreshToken),
		TokenExpires: t.ExpiresAt.UTC().Format(time.RFC3339), ID: a.ID,
	}); err != nil {
		panellog.Error("saving the renewed Cloudflare sign-in of", a.Name+":", err)
	}
	solved(s, "cloudflare:"+a.Name, "Cloudflare account "+a.Name+" renews its sign-in again", "hakobu renewed its access to Cloudflare account "+a.Name+".")
	return t.AccessToken
}

// RenewCloudflareSignIns keeps the OAuth accounts' tokens fresh, every
// interval, whether they're used or not, so a renewal that fails is
// retried long before the token runs out.
func RenewCloudflareSignIns(s *store.Store, interval time.Duration) {
	for {
		if rows, err := s.ListCloudflareAccounts(ctx()); err == nil {
			for _, a := range rows {
				if a.RefreshToken != "" {
					freshToken(s, a)
				}
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
