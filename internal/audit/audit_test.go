package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines as the kernel writes them: address:port in hex, state 0A is LISTEN.
const tables = `  sl  local_address rem_address   st
   0: 0100007F:1F90 00000000:0000 0A
   1: 00000000:0016 00000000:0000 0A
   2: 00000000:1538 00000000:0000 0A
   3: 0100400A:1B9E 00000000:0000 0A
   4: 01004064:1B9E 00000000:0000 0A
   5: 00000000:01BB 00000000:0000 0A
   6: 0A00000A:1F90 0100000A:D431 01
  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:1538 00000000000000000000000000000000:0000 0A
   1: 00000000000000000000000001000000:0050 00000000000000000000000000000000:0000 0A
`

func TestPorts(t *testing.T) {
	got := Ports(tables, []int{22, 443})
	var problems []string
	for _, f := range got {
		problems = append(problems, f.Key+" "+f.Problem)
	}
	all := strings.Join(problems, "\n")
	// 5432 on every address (v4 and v6: one finding), 7070 on 10.64.0.1.
	if len(got) != 2 || !strings.Contains(all, "port:5432 port 5432 listens on every address") || !strings.Contains(all, "port:7070 port 7070 listens on 10.64.0.1") {
		t.Errorf("findings:\n%s", all)
	}
	for _, quiet := range []string{"port:8080", "port:22", "port:443", "port:80"} {
		if strings.Contains(all, quiet+" ") {
			t.Errorf("%s found:\n%s", quiet, all)
		}
	}
}

func TestContainers(t *testing.T) {
	ours := func(n string) bool { return strings.HasPrefix(n, "nova-") || n == "hakobu-postgres" }
	got := Containers([]string{"nova-crm-blue", "hakobu-postgres", "adminer"}, ours)
	if len(got) != 1 || got[0].Key != "container:adminer" {
		t.Errorf("%v", got)
	}
}

func TestUpgrades(t *testing.T) {
	stamp := filepath.Join(t.TempDir(), "upgrade-stamp")
	now := time.Now()
	if got := Upgrades(stamp, now, 72*time.Hour); got != nil {
		t.Errorf("no stamp: %v", got)
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stamp, now.Add(-5*24*time.Hour), now.Add(-5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := Upgrades(stamp, now, 72*time.Hour); len(got) != 1 || !strings.Contains(got[0].Problem, "5 days ago") {
		t.Errorf("old stamp: %v", got)
	}
	if err := os.Chtimes(stamp, now, now); err != nil {
		t.Fatal(err)
	}
	if got := Upgrades(stamp, now, 72*time.Hour); got != nil {
		t.Errorf("fresh stamp: %v", got)
	}
}

func TestOwnerKey(t *testing.T) {
	f := filepath.Join(t.TempDir(), "owner-key")
	if got := OwnerKey(f); got != nil {
		t.Errorf("none: %v", got)
	}
	if err := os.WriteFile(f, []byte("AGE-SECRET-KEY-1…\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OwnerKey(f); len(got) != 1 || got[0].Key != "owner-key" {
		t.Errorf("there: %v", got)
	}
}
