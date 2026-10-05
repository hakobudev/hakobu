package cloudflare

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// "Connect with Cloudflare", for a panel whose server Cloudflare's token
// endpoint answers (dash.cloudflare.com challenges Hetzner and some other
// server networks, see the package comment): authorization code with
// PKCE, no client secret, then refreshing. The panel trades the code and
// refreshes itself; no relay sees the tokens. An API token stays the way
// in everywhere else.

// AuthURL and TokenURL are Cloudflare's OAuth endpoints; tests point them
// at a fake.
var (
	AuthURL  = "https://dash.cloudflare.com/oauth2/auth"
	TokenURL = "https://dash.cloudflare.com/oauth2/token"
)

// oauthScopes are what hakobu does in a connected account: list its
// domains, route them to its tunnel, and make R2 buckets for storages and
// backups. offline_access gets a refresh token.
const oauthScopes = "zone.read dns.write argotunnel.write workers-r2.write offline_access"

// PKCE returns a random code verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL is where the user allows hakobu into their account.
func AuthorizeURL(clientID, redirectURI, state, challenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("scope", oauthScopes)
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	return AuthURL + "?" + v.Encode()
}

// Token is what the token endpoint hands out.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Exchange trades an authorization code for tokens.
func Exchange(clientID, redirectURI, code, verifier string) (Token, error) {
	return tokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {verifier},
	})
}

// Refresh trades a refresh token for new tokens; the refresh token may
// change, so the new one replaces it.
func Refresh(clientID, refreshToken string) (Token, error) {
	t, err := tokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
	if err == nil && t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, err
}

func tokenRequest(form url.Values) (Token, error) {
	req, err := http.NewRequest(http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.Header.Get("cf-mitigated") == "challenge" {
		return Token{}, fmt.Errorf("Cloudflare's sign-in challenges this server's network (cf-mitigated), so it can't trade the code: connect the account with an API token instead")
	}
	var res struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if json.Unmarshal(body, &res) != nil || resp.StatusCode != http.StatusOK || res.AccessToken == "" {
		if res.Error != "" {
			return Token{}, fmt.Errorf("Cloudflare refused the sign-in (%d): %s %s", resp.StatusCode, res.Error, res.Description)
		}
		return Token{}, fmt.Errorf("Cloudflare refused the sign-in (%d): %.200s", resp.StatusCode, body)
	}
	return Token{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(res.ExpiresIn) * time.Second),
	}, nil
}

// Account is a Cloudflare account a token reaches.
type Account struct {
	ID, Name string
	Zones    []string
}

// Accounts are the accounts whose domains the client sees, with them:
// one hakobu can use has a domain.
func (c Client) Accounts() ([]Account, error) {
	zones, err := c.Zones()
	if err != nil {
		return nil, err
	}
	byID := map[string]*Account{}
	var out []*Account
	for _, z := range zones {
		a := byID[z.Account.ID]
		if a == nil {
			a = &Account{ID: z.Account.ID, Name: z.Account.Name}
			byID[z.Account.ID] = a
			out = append(out, a)
		}
		a.Zones = append(a.Zones, z.Name)
	}
	accounts := make([]Account, len(out))
	for i, a := range out {
		accounts[i] = *a
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts, nil
}
