package ops

import (
	"fmt"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

// R2 has to be turned on in a Cloudflare account by its owner, in the
// dashboard (with a payment method, even for the free plan), before any
// bucket can be made there. Pages say so ahead of a storage or a backup
// failing.

// r2OffRecheck is how long "off" holds; "on" holds for good.
const r2OffRecheck = time.Minute

var r2State struct {
	sync.Mutex
	on      map[string]bool
	offSeen map[string]time.Time
}

// R2Off reports whether R2 is known to be off in the account; a check
// that fails otherwise counts as on, so a hiccup shows no warning.
func R2Off(c cloudflare.Client, accountID string) bool {
	if accountID == "" {
		return false
	}
	r2State.Lock()
	if r2State.on[accountID] {
		r2State.Unlock()
		return false
	}
	if t, ok := r2State.offSeen[accountID]; ok && time.Since(t) < r2OffRecheck {
		r2State.Unlock()
		return true
	}
	r2State.Unlock()
	on, err := c.R2On(accountID)
	r2State.Lock()
	defer r2State.Unlock()
	if r2State.on == nil {
		r2State.on, r2State.offSeen = map[string]bool{}, map[string]time.Time{}
	}
	switch {
	case on:
		r2State.on[accountID] = true
		delete(r2State.offSeen, accountID)
	case err == nil:
		r2State.offSeen[accountID] = time.Now()
		return true
	}
	return false
}

// ProjectR2Off is R2Off for the project's account, with its R2 page.
func ProjectR2Off(s *store.Store, p store.Project) (off bool, url string) {
	a, err := projectAccount(s, p)
	if err != nil {
		return false, ""
	}
	return R2Off(a.Client, a.AccountID), cloudflare.R2URL(a.AccountID)
}

// PanelR2Off is R2Off for the panel's account, with its R2 page.
func PanelR2Off(s *store.Store) (off bool, url string) {
	a, err := panelAccount(s)
	if err != nil {
		return false, ""
	}
	return R2Off(a.Client, a.AccountID), cloudflare.R2URL(a.AccountID)
}

// r2Explained says, for an R2 call refused because R2 is off in the
// account (named by where), what to do; other errors pass through.
func r2Explained(accountID, where string, err error) error {
	if !cloudflare.R2Off(err) {
		return err
	}
	r2State.Lock()
	delete(r2State.on, accountID)
	r2State.Unlock()
	return fmt.Errorf("R2 isn't turned on in %s: its owner turns it on at %s (it asks for a payment method; 10 GB are free), then try again", where, cloudflare.R2URL(accountID))
}
