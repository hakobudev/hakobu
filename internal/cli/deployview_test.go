package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
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
		{deployed{Result: apply.Result{Project: "crm"}}, []string{"crm  nova-1  ● nothing to change"}},
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
