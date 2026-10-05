package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/store"
)

// A domain that lapses takes the panel or the apps on it down with it, and
// its registrar's reminders may go to an old inbox. Once a day hakobu asks
// the registries (RDAP, which every gTLD and most ccTLDs answer) when the
// domains of the panel and the apps expire, and warns the owner from 30
// days before: on the home page, and by email at 30, 7 and 1 days.

// rdapBootstrapURL lists the RDAP server of each TLD; tests point it at a
// fake.
var rdapBootstrapURL = "https://data.iana.org/rdap/dns.json"

const (
	domainWarnBefore = 30 * 24 * time.Hour
	domainCheckEvery = 24 * time.Hour
)

// DomainExpiry is a domain hakobu uses that expires soon.
type DomainExpiry struct {
	Domain  string
	Expires time.Time
	Uses    []string // the panel, apps
}

// Days left, negative once it has expired.
func (d DomainExpiry) Days() int {
	return int(time.Until(d.Expires).Hours() / 24)
}

func (d DomainExpiry) When() string {
	switch n := d.Days(); {
	case n < 0:
		return fmt.Sprintf("expired %d %s ago", -n, plural(-n, "day", "days"))
	case n == 0:
		return "expires today"
	default:
		return fmt.Sprintf("expires in %d %s", n, plural(n, "day", "days"))
	}
}

var domainState struct {
	sync.Mutex
	checked  time.Time
	expiring []DomainExpiry
	servers  map[string]string // RDAP base URL by TLD
	listed   time.Time
}

// ExpiringDomains are the domains found expiring within 30 days, soonest
// first.
func ExpiringDomains() []DomainExpiry {
	domainState.Lock()
	defer domainState.Unlock()
	return slices.Clone(domainState.expiring)
}

// watchedDomains are the registered domains of the panel and the apps,
// with what uses each.
func watchedDomains(s *store.Store) map[string][]string {
	out := map[string][]string{}
	add := func(host, use string) {
		d, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(strings.TrimSuffix(host, ".")))
		if err == nil && !slices.Contains(out[d], use) {
			out[d] = append(out[d], use)
		}
	}
	if host := config.PublicHost(); host != "" {
		add(host, "the panel")
	}
	apps, _ := s.ListApps(ctx())
	for _, a := range apps {
		if a.Domain != "" {
			add(a.Domain, "app "+a.Name)
		}
	}
	return out
}

// CheckDomains asks the registries about the domains in use, once a day.
func CheckDomains(s *store.Store) {
	domainState.Lock()
	if time.Since(domainState.checked) < domainCheckEvery {
		domainState.Unlock()
		return
	}
	domainState.checked = time.Now()
	domainState.Unlock()

	var expiring []DomainExpiry
	for domain, uses := range watchedDomains(s) {
		at, err := domainExpiry(domain)
		if err != nil {
			panellog.Warn("expiry of", domain+":", err)
			continue
		}
		if time.Until(at) > domainWarnBefore {
			continue
		}
		d := DomainExpiry{Domain: domain, Expires: at, Uses: uses}
		expiring = append(expiring, d)
		bucket := "30"
		switch days := d.Days(); {
		case days <= 1:
			bucket = "1"
		case days <= 7:
			bucket = "7"
		}
		problem(s, "domain:"+domain+":"+bucket, 365*24*time.Hour, domain+" "+d.When(),
			fmt.Sprintf("%s %s (%s). Renew it at its registrar: if it lapses, these stop answering: %s.\n\n%s",
				domain, d.When(), at.UTC().Format("2006-01-02"), strings.Join(uses, ", "), panelURL("/")))
	}
	slices.SortFunc(expiring, func(a, b DomainExpiry) int { return a.Expires.Compare(b.Expires) })
	domainState.Lock()
	domainState.expiring = expiring
	domainState.Unlock()
}

var rdapClient = &http.Client{Timeout: 20 * time.Second}

// domainExpiry asks the domain's registry when it expires.
func domainExpiry(domain string) (time.Time, error) {
	base, err := rdapServer(domain)
	if err != nil {
		return time.Time{}, err
	}
	var d struct {
		Events []struct {
			Action string    `json:"eventAction"`
			Date   time.Time `json:"eventDate"`
		} `json:"events"`
	}
	if err := getRDAP(strings.TrimSuffix(base, "/")+"/domain/"+domain, &d); err != nil {
		return time.Time{}, err
	}
	for _, e := range d.Events {
		if e.Action == "expiration" {
			return e.Date, nil
		}
	}
	return time.Time{}, fmt.Errorf("the registry gives no expiration date")
}

// rdapServer is the RDAP server for the domain's TLD, from IANA's list
// (fetched once a week).
func rdapServer(domain string) (string, error) {
	domainState.Lock()
	defer domainState.Unlock()
	if domainState.servers == nil || time.Since(domainState.listed) > 7*24*time.Hour {
		var boot struct {
			Services [][][]string `json:"services"`
		}
		if err := getRDAP(rdapBootstrapURL, &boot); err != nil {
			return "", fmt.Errorf("the list of RDAP servers: %w", err)
		}
		servers := map[string]string{}
		for _, svc := range boot.Services {
			if len(svc) == 2 && len(svc[1]) > 0 {
				for _, tld := range svc[0] {
					servers[strings.ToLower(tld)] = svc[1][0]
				}
			}
		}
		domainState.servers, domainState.listed = servers, time.Now()
	}
	// The longest listed suffix: some registries serve second-level ones.
	for name := domain; name != ""; {
		if base, ok := domainState.servers[name]; ok {
			return base, nil
		}
		_, rest, ok := strings.Cut(name, ".")
		if !ok {
			break
		}
		name = rest
	}
	return "", fmt.Errorf("its registry has no RDAP server")
}

func getRDAP(url string, v any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	resp, err := rdapClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
