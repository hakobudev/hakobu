package node

import (
	"strings"
	"testing"
)

func TestTailnetRequests(t *testing.T) {
	up := TailnetRequest{Up: true, Key: "tskey-auth-k1CNTRL-abc123", Hostname: "hakobu-kasl", Tag: "tag:hakobu"}
	for _, r := range []TailnetRequest{up, {}} {
		got, err := ParseTailnetRequest(r.String())
		if err != nil || got != r {
			t.Errorf("%q → %+v (%v)", r.String(), got, err)
		}
	}
	for _, s := range []string{
		"", "up", "down now",
		"up tskey-auth-x hakobu-kasl tag:hakobu --ssh",
		"up tskey-api-x hakobu-kasl tag:hakobu",             // not an auth key
		"up --exit-node=1.2.3.4 hakobu-kasl tag:hakobu",     // a flag for a key
		"up tskey-auth-x --accept-routes tag:hakobu",        // a flag for a hostname
		"up tskey-auth-x hakobu-kasl --advertise-exit-node", // a flag for a tag
		"up tskey-auth-x;reboot hakobu-kasl tag:hakobu",
		"up tskey-auth-x Hakobu_Kasl tag:hakobu",
		"up tskey-auth-x hakobu-kasl hakobu",
		"up tskey-auth-" + strings.Repeat("a", 300) + " hakobu-kasl tag:hakobu",
	} {
		if r, err := ParseTailnetRequest(s); err == nil {
			t.Errorf("%q passed: %+v", s, r)
		}
	}
}
