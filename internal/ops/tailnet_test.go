package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/tailscale"
)

// fakeTailnet is Tailscale's API for client "id"/"secret" with tag:hakobu
// and one device, hakobu-box at 100.78.99.72.
func fakeTailnet(t *testing.T) *[]string {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch route := r.Method + " " + r.URL.Path; {
		case route == "POST /oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"invalid client"}`)
				return
			}
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case route == "POST /tailnet/-/keys":
			fmt.Fprint(w, `{"key":"tskey-auth-k1-abc"}`)
		case route == "GET /tailnet/-/devices":
			fmt.Fprint(w, `{"devices":[{"nodeId":"n1","hostname":"hakobu-box","addresses":["100.78.99.72"],"connectedToControl":true}]}`)
		case strings.HasPrefix(route, "DELETE /device/"):
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/device/"))
		default:
			t.Errorf("unexpected %s", route)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := tailscale.APIURL
	tailscale.APIURL = srv.URL
	t.Cleanup(func() { tailscale.APIURL = old })
	tailnetDevices.Lock()
	tailnetDevices.m = map[string]cachedDevices{}
	tailnetDevices.Unlock()
	return &deleted
}

func TestServerTailnet(t *testing.T) {
	deleted := fakeTailnet(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := nodetest.New()
	joinServer(t, s, "box", f)

	if err := ConnectServerTailnet(s, 0, "box", ""); err == nil || !strings.Contains(err.Error(), "Settings → Tailscale") {
		t.Errorf("without a client: %v", err)
	}
	if err := SetTailscaleClient(s, 0, "id", "wrong", ""); err == nil {
		t.Error("a client with a wrong secret was kept")
	}
	if err := SetTailscaleClient(s, 0, "id", "secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetTailscaleClient(s, 0, "id", "", "tag:hakobu"); err != nil {
		t.Errorf("saving again without the secret: %v", err)
	}
	if c, ok := TailscaleClientOf(s, 0); !ok || c.Tag != DefaultTailnetTag || c.Secret != "secret" {
		t.Errorf("client = %+v", c)
	}

	if err := ConnectServerTailnet(s, 0, "box", ""); err != nil {
		t.Fatal(err)
	}
	want := node.TailnetRequest{Up: true, Key: "tskey-auth-k1-abc", Hostname: "hakobu-box", Tag: "tag:hakobu"}
	if len(f.TailnetAsk) != 1 || f.TailnetAsk[0] != want {
		t.Fatalf("asked %+v, want %+v", f.TailnetAsk, want)
	}

	// Root did it: the server has its address, and its device is found.
	f.InTailnet = node.TailnetState{IP: "100.78.99.72"}
	n, _ := UserServer(s, 0, "box")
	if st := TailnetOf(s, n); !st.Known || !st.Device || !st.Online {
		t.Errorf("state = %+v", st)
	}
	if n, _ = UserServer(s, 0, "box"); n.TailnetDevice != "n1" || n.TailnetIP != "100.78.99.72" {
		t.Errorf("kept device %q at %q", n.TailnetDevice, n.TailnetIP)
	}
	// Away, it shows where it was.
	f.Fail("Tailnet", node.ErrUnreachable)
	if st := TailnetOf(s, n); st.Known || st.IP != "100.78.99.72" {
		t.Errorf("away: state = %+v", st)
	}
	f.Fail("Tailnet", nil)
	if err := ConnectServerTailnet(s, 0, "box", ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("connecting again: %v", err)
	}

	if err := DisconnectServerTailnet(s, 0, "box"); err != nil {
		t.Fatal(err)
	}
	if len(f.TailnetAsk) != 2 || f.TailnetAsk[1].Up {
		t.Errorf("asked %+v, want down last", f.TailnetAsk)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "n1" {
		t.Errorf("deleted %v, want n1", *deleted)
	}
	if n, _ = UserServer(s, 0, "box"); n.TailnetDevice != "" || n.TailnetIP != "" {
		t.Errorf("still kept device %q at %q", n.TailnetDevice, n.TailnetIP)
	}

	// A server told apart from the rest by a tag of its own.
	f.InTailnet = node.TailnetState{}
	if err := ConnectServerTailnet(s, 0, "box", "tag:kasl-server"); err != nil {
		t.Fatal(err)
	}
	if last := f.TailnetAsk[len(f.TailnetAsk)-1]; last.Tag != "tag:kasl-server" {
		t.Errorf("joined with %q, want tag:kasl-server", last.Tag)
	}
	if err := ConnectServerTailnet(s, 0, "box", "kasl-server"); err == nil {
		t.Error("joined with a malformed tag")
	}
}

// A server put in the tailnet by hand is taken as it is.
func TestServerTailnetByHand(t *testing.T) {
	fakeTailnet(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := nodetest.New()
	f.InTailnet = node.TailnetState{IP: "100.78.99.72"}
	joinServer(t, s, "box", f)
	if err := SetTailscaleClient(s, 0, "id", "secret", ""); err != nil {
		t.Fatal(err)
	}
	n, _ := UserServer(s, 0, "box")
	if st := TailnetOf(s, n); !st.Device || st.IP != "100.78.99.72" {
		t.Errorf("state = %+v", st)
	}
	if len(f.TailnetAsk) != 0 {
		t.Errorf("root was asked %+v", f.TailnetAsk)
	}
}
