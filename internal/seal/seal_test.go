package seal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func keys(t *testing.T) (server, owner, stranger *age.X25519Identity) {
	t.Helper()
	ids := make([]*age.X25519Identity, 3)
	for i := range ids {
		id, err := NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids[0], ids[1], ids[2]
}

func TestSealOpen(t *testing.T) {
	server, owner, _ := keys(t)
	s, err := New(server.Recipient().String(), owner.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("kasl", "crm", "STRIPE_KEY", "sk_live_123"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("kasl", "hakobu", StatusToken, "github_pat_x"); err != nil {
		t.Fatal(err)
	}
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk_live") || strings.Contains(string(b), "github_pat") {
		t.Fatalf("a value is in the clear:\n%s", b)
	}
	if !strings.Contains(string(b), "STRIPE_KEY") {
		t.Errorf("the name isn't visible:\n%s", b)
	}
	read, err := Parse(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	for _, id := range []*age.X25519Identity{server, owner} {
		o, err := read.Open(id, "kasl")
		if err != nil {
			t.Fatal(err)
		}
		if o.Apps["crm"]["STRIPE_KEY"] != "sk_live_123" || o.Hakobu[StatusToken] != "github_pat_x" {
			t.Errorf("opened %+v", o)
		}
	}
}

func TestOpenByStranger(t *testing.T) {
	server, owner, stranger := keys(t)
	s, _ := New(server.Recipient().String(), owner.Recipient().String())
	_ = s.Set("kasl", "crm", "KEY", "v")
	_, err := s.Open(stranger, "kasl")
	if !errors.Is(err, ErrNotARecipient) || !strings.Contains(err.Error(), "apps.crm.KEY") {
		t.Errorf("err = %v", err)
	}
}

func TestOpensOnlyWhereSealed(t *testing.T) {
	server, _, _ := keys(t)
	s, _ := New(server.Recipient().String())
	_ = s.Set("kasl", "crm", "STRIPE_KEY", "sk_live")
	stripe := s.Apps["crm"]["STRIPE_KEY"]
	// Pasted into another project's file, or as another app's or variable.
	for _, at := range []struct{ project, app, name string }{{"shop", "crm", "STRIPE_KEY"}, {"kasl", "web", "STRIPE_KEY"}, {"kasl", "crm", "OTHER"}} {
		other, _ := New(server.Recipient().String())
		other.Apps = map[string]map[string]string{at.app: {at.name: stripe}}
		if _, err := other.Open(server, at.project); err == nil || !strings.Contains(err.Error(), "sealed for kasl/crm/STRIPE_KEY") {
			t.Errorf("%v: %v", at, err)
		}
	}
	// One sealed bare, by hakobu before 1.6, opens, and says so.
	bare, _ := s.Seal("old")
	s.Apps["crm"]["OLD"] = bare
	o, err := s.Open(server, "kasl")
	if err != nil || o.Apps["crm"]["OLD"] != "old" || o.Apps["crm"]["STRIPE_KEY"] != "sk_live" {
		t.Fatalf("%+v, %v", o, err)
	}
	if len(o.Bare) != 1 || o.Bare[0] != (Bare{"crm", "OLD", bare}) {
		t.Errorf("bare: %+v", o.Bare)
	}
}

func TestSameValueSealsDifferently(t *testing.T) {
	server, _, _ := keys(t)
	s, _ := New(server.Recipient().String())
	_ = s.Set("kasl", "a", "X", "same")
	_ = s.Set("kasl", "a", "Y", "same")
	if s.Apps["a"]["X"] == s.Apps["a"]["Y"] {
		t.Error("equal values give equal ciphertexts, which tells that they're equal")
	}
}

func TestParseRefuses(t *testing.T) {
	server, _, _ := keys(t)
	r := `recipients = ["` + server.Recipient().String() + `"]` + "\n"
	for _, c := range []struct{ name, toml, want string }{
		{"no recipients", "[apps.crm]\nKEY = \"age:x\"", "nobody could read"},
		{"bad recipient", `recipients = ["ssh-ed25519 AAAA"]`, "isn't an age public key"},
		{"plain value", r + "[apps.crm]\nKEY = \"hunter2\"", "not sealed"},
		{"bad name", r + "[apps.crm]\nkey = \"age:x\"", "not a variable name"},
		{"unknown hakobu", r + "[hakobu]\ntoken = \"age:x\"", "no such secret"},
		{"typo", r + "[app.crm]\nKEY = \"age:x\"", "unknown keys"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.toml)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestSetRefuses(t *testing.T) {
	server, _, _ := keys(t)
	s, _ := New(server.Recipient().String())
	if err := s.Set("kasl", "crm", "lower", "v"); err == nil {
		t.Error("a lowercase name was taken")
	}
	if err := s.Set("kasl", "hakobu", "nope", "v"); err == nil {
		t.Error("an unknown hakobu secret was taken")
	}
}

func TestUnset(t *testing.T) {
	server, _, _ := keys(t)
	s, _ := New(server.Recipient().String())
	_ = s.Set("kasl", "crm", "A", "1")
	if !s.Unset("crm", "A") || s.Unset("crm", "A") || s.Apps["crm"] != nil {
		t.Errorf("apps %v", s.Apps)
	}
}

func TestIdentityFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.age")
	id, _ := NewIdentity()
	if err := SaveIdentity(file, id); err != nil {
		t.Fatal(err)
	}
	if err := SaveIdentity(file, id); err == nil {
		t.Error("an identity file was replaced")
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	back, err := LoadIdentity(file)
	if err != nil || back.String() != id.String() {
		t.Errorf("read back %v, %v", back, err)
	}
}

func TestCheckApps(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	s, _ := New(id.Recipient().String())
	_ = s.Set("kasl", "web", "KEY", "v")
	_ = s.Set("kasl", "wbe", "KEY", "v")
	err := s.CheckApps([]string{"api", "web"})
	if err == nil || !strings.Contains(err.Error(), "apps.wbe: hakobu.toml has no such app") || strings.Contains(err.Error(), "apps.web") {
		t.Errorf("err %v", err)
	}
	_ = s.Unset("wbe", "KEY")
	if err := s.CheckApps([]string{"web"}); err != nil {
		t.Error(err)
	}
}
