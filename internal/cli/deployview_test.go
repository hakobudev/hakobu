package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/server"
)

func TestPrintDeploy(t *testing.T) {
	ok := deployed{
		Result: apply.Result{Project: "crm", Revision: strings.Repeat("a1f3c9e", 6), Changes: []string{"update app web"}},
		Steps: []apply.Step{
			{Name: "plan", Detail: "web"},
			{Name: "pull", Detail: "web · ghcr.io/nova/crm-web", Took: 3200 * time.Millisecond},
			{Name: "switch", Detail: "web"},
		},
	}
	failed := deployed{
		Result: apply.Result{Project: "crm", Error: "apps.web: release failed: PG::UndefinedColumn; the live version keeps running", Changes: []string{"update app web"}},
		Steps:  []apply.Step{{Name: "release", Detail: "web · bin/rails db:migrate", Error: "release failed: PG::UndefinedColumn"}},
	}
	for _, c := range []struct {
		d    deployed
		want []string
	}{
		{ok, []string{"crm  a1f3c9e  nova-1", "  ● pull     web · ghcr.io/nova/crm-web · 3.2 s", "● deployed in 3.2 s"}},
		{failed, []string{"  ✕ release  web · bin/rails db:migrate", "             release failed: PG::UndefinedColumn", "✕ not deployed · the live version keeps running"}},
		{deployed{Result: apply.Result{Project: "crm"}}, []string{"crm  nova-1", "● nothing to change"}},
	} {
		var b bytes.Buffer
		printDeploy(&b, "nova-1", c.d)
		for _, w := range c.want {
			if !strings.Contains(b.String(), w+"\n") {
				t.Errorf("no %q in\n%s", w, b.String())
			}
		}
	}
}

// A command an SSH key ran is in colour without a terminal.
func TestLookOverSSH(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if lookFor(os.Stdout).on && !overSSH {
		t.Skip("stdout is a terminal")
	}
	overSSH = true
	t.Cleanup(func() { overSSH = false })
	if !lookFor(&bytes.Buffer{}).on {
		t.Error("no colour over ssh")
	}
	t.Setenv("NO_COLOR", "1")
	if lookFor(&bytes.Buffer{}).on {
		t.Error("NO_COLOR ignored")
	}
}

// At a terminal a step shows as it starts, and its end takes its line.
func TestDeployViewLive(t *testing.T) {
	var b bytes.Buffer
	v := &deployView{w: &b, l: look{true}, name: "nova-1"}
	failed := false
	for _, e := range []string{
		`{"start":{"project":"crm","revision":"a1f3c9e"}}`,
		`{"step":{"name":"release","detail":"web · migrate"}}`,
		`{"step":{"name":"release","detail":"web · migrate","error":"exited 1"},"done":true}`,
		`{"result":{"project":"crm","error":"apps.web: exited 1","changes":["x"],"steps":[{"name":"release","error":"exited 1"}]}}`,
	} {
		if err := v.event([]byte(e), &failed); err != nil {
			t.Fatal(err)
		}
	}
	out := b.String()
	if !failed || !strings.Contains(out, "◌\x1b[0m release  web · migrate\r\x1b[2K  \x1b[1;91m✕\x1b[0m release") || !strings.Contains(out, "not deployed") {
		t.Errorf("failed %v, out %q", failed, out)
	}
	if err := v.event([]byte(`{"error":"registry gone"}`), &failed); err == nil || err.Error() != "nova-1: registry gone" {
		t.Errorf("err = %v", err)
	}
}

func TestStatusSizes(t *testing.T) {
	var b bytes.Buffer
	printStatus(&b, server.Status{Server: "nova-1", Resources: []server.ResourceStatus{
		{Project: "crm", Kind: "database", Name: "main", Size: 1288490188},
		{Project: "crm", Kind: "cache", Name: "cache", Size: 12 << 20, Limit: 64 << 20},
		{Project: "crm", Kind: "cache", Name: "jobs"},
	}})
	for _, w := range []string{"main   postgres  1.2 GB\n", "cache  valkey    12 MB of 64 MB\n", "jobs   valkey\n"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("no %q in\n%s", w, b.String())
		}
	}
}

// A running step's line fits the terminal, so its end can take it back.
func TestDeployViewLongStep(t *testing.T) {
	var b bytes.Buffer
	v := &deployView{w: &b, l: look{true}, name: "nova-1"}
	v.step(apply.Step{Name: "release", Detail: "web · " + strings.Repeat("x", 200)}, false)
	if n := len([]rune(stripANSI(b.String()))); n > 80 {
		t.Errorf("%d columns: %q", n, b.String())
	}
}

func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		s = s[:i] + s[i+j+1:]
	}
}
