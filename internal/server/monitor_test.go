package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/spec"
)

type inbox struct{ subjects []string }

func (i *inbox) Send(subject, body string) error {
	i.subjects = append(i.subjects, subject)
	return nil
}

func (i *inbox) last() string {
	if len(i.subjects) == 0 {
		return ""
	}
	return i.subjects[len(i.subjects)-1]
}

// watched is a daemon with an app deployed on a fake machine, mailing box.
func watched(t *testing.T) (*Daemon, *nodetest.Fake, *inbox) {
	d, _ := newDaemon(t)
	f := d.Machine.(machine).Fake
	box := &inbox{}
	d.Alerts.Mailer = box
	d.Applier.Pull = func(_ context.Context, ref, tag string) error {
		app := strings.TrimSuffix(strings.TrimPrefix(tag, "hakobu/"), ":next")
		if f.Images[app] == nil {
			f.Images[app] = map[node.Image]string{}
		}
		f.Images[app][node.Next] = ref
		return nil
	}
	p, err := spec.Parse([]byte("project = \"nova\"\nserver = \"nova-1\"\n[apps.crm]\nuses = [\"main\"]\n[databases.main]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Applier.Apply(context.Background(), apply.Release{Spec: p, Images: map[string]string{"crm": "r/crm@sha256:1"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	return d, f, box
}

func TestAppDown(t *testing.T) {
	d, f, box := watched(t)
	ctx := context.Background()
	misses := map[string]int{}
	d.checkApps(ctx, misses)
	f.Fail("CheckHealth", errors.New("didn't return 2xx on port 8080/up"))
	d.checkApps(ctx, misses)
	d.checkApps(ctx, misses)
	if len(box.subjects) != 0 {
		t.Fatalf("mailed after 2 misses: %q", box.subjects)
	}
	d.checkApps(ctx, misses)
	d.checkApps(ctx, misses)
	if len(box.subjects) != 1 || !strings.Contains(box.last(), "nova/crm is down: didn't return 2xx") {
		t.Fatalf("after 3 misses: %q", box.subjects)
	}
	f.Fail("CheckHealth", nil)
	d.checkApps(ctx, misses)
	if len(box.subjects) != 2 || !strings.Contains(box.last(), "resolved: nova/crm is down") {
		t.Errorf("after it answered: %q", box.subjects)
	}
}

func TestDiskAndPostgres(t *testing.T) {
	d, f, box := watched(t)
	ctx := context.Background()
	m := d.Machine.(machine)
	m.used, m.total = 90, 100
	d.Machine = m
	d.checkDisk(ctx)
	if !strings.Contains(box.last(), "the disk is 90% full") {
		t.Fatalf("%q", box.subjects)
	}
	m.used = 82 // between the marks: stays open
	d.Machine = m
	d.checkDisk(ctx)
	m.used = 70
	d.Machine = m
	d.checkDisk(ctx)
	if len(box.subjects) != 2 || !strings.Contains(box.last(), "resolved: the disk") {
		t.Errorf("%q", box.subjects)
	}
	f.Fail("PostgresReady", errors.New("down"))
	d.checkPostgres(ctx)
	if !strings.Contains(box.last(), "PostgreSQL doesn't answer") {
		t.Errorf("%q", box.subjects)
	}
}

func TestAuditContainers(t *testing.T) {
	d, _, box := watched(t)
	ctx := context.Background()
	running := []string{"nova-crm-green", node.PostgresContainer, "adminer"}
	d.Containers = func(context.Context) ([]string, error) { return running, nil }
	d.Audit(ctx)
	mailed := strings.Join(box.subjects, "\n")
	if !strings.Contains(mailed, "container adminer runs, and hakobu didn't start it") || strings.Contains(mailed, "nova-crm-green") {
		t.Fatalf("%s", mailed)
	}
	// Docker not answering isn't adminer gone.
	d.Containers = func(context.Context) ([]string, error) { return nil, errors.New("docker restarting") }
	d.Audit(ctx)
	if strings.Contains(strings.Join(box.subjects, "\n"), "resolved") {
		t.Fatalf("closed on a failed check: %q", box.subjects)
	}
	d.Containers = func(context.Context) ([]string, error) { return running, nil }
	running = running[:2]
	d.Audit(ctx)
	if !strings.Contains(strings.Join(box.subjects, "\n"), "resolved: container adminer") {
		t.Errorf("%q", box.subjects)
	}
}

func TestDeployAlerts(t *testing.T) {
	d, _, box := watched(t)
	d.Watch.Done(apply.Meta{}, apply.Result{Release: "ghcr.io/nova/rel@sha256:1", Error: "not found"})
	if !strings.Contains(box.last(), "deploy of ghcr.io/nova/rel failed: not found") {
		t.Fatalf("%q", box.subjects)
	}
	d.Watch.Done(apply.Meta{}, apply.Result{Release: "ghcr.io/nova/rel@sha256:2", Project: "nova"})
	if !strings.Contains(box.last(), "resolved: deploy of ghcr.io/nova/rel") {
		t.Fatalf("a failure before the project was known stays open: %q", box.subjects)
	}
	d.Watch.Done(apply.Meta{}, apply.Result{Release: "ghcr.io/nova/rel@sha256:3", Project: "nova", Error: "apps.crm: release command: exit 3\nmore"})
	if !strings.Contains(box.last(), "deploy of nova failed: apps.crm: release command: exit 3") {
		t.Fatalf("%q", box.subjects)
	}
	d.Watch.Done(apply.Meta{}, apply.Result{Release: "ghcr.io/nova/rel@sha256:4", Project: "nova"})
	if !strings.Contains(box.last(), "resolved: deploy of nova failed") {
		t.Errorf("%q", box.subjects)
	}
}
