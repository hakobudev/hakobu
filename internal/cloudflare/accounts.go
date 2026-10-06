package cloudflare

import "sort"

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
