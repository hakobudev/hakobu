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

// fakeTailnets is Tailscale's API with two tailnets: client "home"
// (secret "home-secret") with device n1 at 100.78.99.72, and client
// "work" (secret "work-secret") with device n2 at 100.109.0.5. Deleted
// devices are listed as "<client>/<node>".
func fakeTailnets(t *testing.T) *[]string {
	var deleted []string
	devices := map[string]string{
		"home": `{"devices":[{"nodeId":"n1","hostname":"hakobu-box","addresses":["100.78.99.72"],"connectedToControl":true}]}`,
		"work": `{"devices":[{"nodeId":"n2","hostname":"hakobu-box","addresses":["100.109.0.5"],"tags":["tag:kasl-server"],"connectedToControl":true}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			id := r.Form.Get("client_id")
			if _, ok := devices[id]; !ok || r.Form.Get("client_secret") != id+"-secret" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"invalid client"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":"tok-%s","expires_in":3600}`, id)
			return
		}
		client := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		switch route := r.Method + " " + r.URL.Path; {
		case route == "POST /tailnet/-/keys":
			fmt.Fprintf(w, `{"key":"tskey-auth-%s-abc"}`, client)
		case route == "GET /tailnet/-/devices":
			fmt.Fprint(w, devices[client])
		case strings.HasPrefix(route, "DELETE /device/"):
			deleted = append(deleted, client+"/"+strings.TrimPrefix(r.URL.Path, "/device/"))
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

func tailnetStore(t *testing.T) (*store.Store, *nodetest.Fake) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := nodetest.New()
	joinServer(t, s, "box", f)
	return s, f
}

func TestServerTailnet(t *testing.T) {
	deleted := fakeTailnets(t)
	s, f := tailnetStore(t)

	if err := ConnectServerTailnet(s, 0, "box", "", ""); err == nil || !strings.Contains(err.Error(), "Settings → Tailscale") {
		t.Errorf("without a tailnet: %v", err)
	}
	if err := AddTailnet(s, 0, "home", "home", "wrong", ""); err == nil {
		t.Error("a client with a wrong secret was kept")
	}
	if err := AddTailnet(s, 0, "Home Net", "home", "home-secret", ""); err == nil {
		t.Error("a tailnet with a malformed name was kept")
	}
	if err := AddTailnet(s, 0, "home", "home", "home-secret", ""); err != nil {
		t.Fatal(err)
	}
	if err := AddTailnet(s, 0, "home", "work", "work-secret", ""); err == nil {
		t.Error("two tailnets of one name")
	}
	if all := Tailnets(s, 0); len(all) != 1 || all[0].Client.Tag != DefaultTailnetTag {
		t.Errorf("tailnets = %+v", all)
	}

	// With one tailnet, it needn't be named.
	if err := ConnectServerTailnet(s, 0, "box", "", ""); err != nil {
		t.Fatal(err)
	}
	want := node.TailnetRequest{Up: true, Key: "tskey-auth-home-abc", Hostname: "hakobu-box", Tag: "tag:hakobu"}
	if len(f.TailnetAsk) != 1 || f.TailnetAsk[0] != want {
		t.Fatalf("asked %+v, want %+v", f.TailnetAsk, want)
	}

	// Root did it: the server has its address, and its device is found.
	f.InTailnet = node.TailnetState{IP: "100.78.99.72"}
	n, _ := UserServer(s, 0, "box")
	if st := TailnetOf(s, n); !st.Known || !st.Device || !st.Online || st.Tailnet != "home" {
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
	if err := ConnectServerTailnet(s, 0, "box", "", ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("connecting again: %v", err)
	}

	if err := DisconnectServerTailnet(s, 0, "box"); err != nil {
		t.Fatal(err)
	}
	if len(f.TailnetAsk) != 2 || f.TailnetAsk[1].Up {
		t.Errorf("asked %+v, want down last", f.TailnetAsk)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "home/n1" {
		t.Errorf("deleted %v, want home/n1", *deleted)
	}
	if n, _ = UserServer(s, 0, "box"); n.TailnetDevice != "" || n.TailnetIP != "" || n.TailnetID.Valid {
		t.Errorf("still kept device %q at %q in %v", n.TailnetDevice, n.TailnetIP, n.TailnetID)
	}
}

// Two tailnets: a server joins the one picked, with a tag of its own,
// and is found and removed there.
func TestServerTailnets(t *testing.T) {
	deleted := fakeTailnets(t)
	s, f := tailnetStore(t)
	for _, name := range []string{"home", "work"} {
		if err := AddTailnet(s, 0, name, name, name+"-secret", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := ConnectServerTailnet(s, 0, "box", "", ""); err == nil || !strings.Contains(err.Error(), "pick") {
		t.Errorf("no tailnet picked of two: %v", err)
	}
	if err := ConnectServerTailnet(s, 0, "box", "nowhere", ""); err == nil {
		t.Error("joined a tailnet the user doesn't have")
	}
	if err := ConnectServerTailnet(s, 0, "box", "work", "tag:kasl-server"); err != nil {
		t.Fatal(err)
	}
	if got := f.TailnetAsk[0]; got.Key != "tskey-auth-work-abc" || got.Tag != "tag:kasl-server" {
		t.Errorf("asked %+v", got)
	}
	if err := ConnectServerTailnet(s, 0, "box", "work", "kasl-server"); err == nil {
		t.Error("joined with a malformed tag")
	}

	f.InTailnet = node.TailnetState{IP: "100.109.0.5"}
	n, _ := UserServer(s, 0, "box")
	if st := TailnetOf(s, n); st.Tailnet != "work" || len(st.Tags) != 1 || st.Tags[0] != "tag:kasl-server" {
		t.Errorf("state = %+v", st)
	}
	if err := RenameTailnet(s, 0, "work", "kasl"); err != nil {
		t.Fatal(err)
	}
	if err := RenameTailnet(s, 0, "kasl", "home"); err == nil {
		t.Error("renamed onto another tailnet's name")
	}
	if err := DisconnectServerTailnet(s, 0, "box"); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "work/n2" {
		t.Errorf("deleted %v, want work/n2", *deleted)
	}

	// Forgotten, a tailnet's servers no longer point at it.
	f.InTailnet = node.TailnetState{}
	if err := ConnectServerTailnet(s, 0, "box", "home", ""); err != nil {
		t.Fatal(err)
	}
	if err := ForgetTailnet(s, 0, "home"); err != nil {
		t.Fatal(err)
	}
	if n, _ = UserServer(s, 0, "box"); n.TailnetID.Valid {
		t.Errorf("still in forgotten tailnet %v", n.TailnetID)
	}
	if all := Tailnets(s, 0); len(all) != 1 || all[0].Name != "kasl" {
		t.Errorf("tailnets = %+v", all)
	}
}

// A server put in a tailnet by hand is found in whichever it is.
func TestServerTailnetByHand(t *testing.T) {
	fakeTailnets(t)
	s, f := tailnetStore(t)
	f.InTailnet = node.TailnetState{IP: "100.109.0.5"}
	for _, name := range []string{"home", "work"} {
		if err := AddTailnet(s, 0, name, name, name+"-secret", ""); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := UserServer(s, 0, "box")
	if st := TailnetOf(s, n); !st.Device || st.Tailnet != "work" {
		t.Errorf("state = %+v", st)
	}
	if len(f.TailnetAsk) != 0 {
		t.Errorf("root was asked %+v", f.TailnetAsk)
	}
}
