package node

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

func envelope(payload string) []byte {
	return fmt.Appendf(nil, "{}\n{\"type\":\"event\",\"length\":%d}\n%s\n", len(payload), payload)
}

// The same error sent again is one issue; resolving it takes it off the
// open ones until it's seen again.
func TestIssues(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	tel := Telemetry{Q: s.Tel}
	const app = 1
	dump := func(n int) {
		t.Helper()
		e := fmt.Sprintf(`{"level":"error","logger":"src.phone.executor","logentry":{"message":"2026-10-14 09:%02d:07.716 | ERROR | src.phone.executor:upload_xml_dump:26 - UI dump: https://r2.example.com/ui_dumps/%d.xml"}}`, n, n)
		if err := tel.StoreEnvelope(ctx, app, envelope(e)); err != nil {
			t.Fatal(err)
		}
	}
	open := func() []teldb.ListIssuesRow {
		t.Helper()
		issues, err := tel.AppIssues(ctx, app, false, 50)
		if err != nil {
			t.Fatal(err)
		}
		return issues
	}

	for n := range 3 {
		dump(n)
	}
	if err := tel.StoreEnvelope(ctx, app, envelope(`{"message":"something else"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tel.AddAppEvent(ctx, teldb.CreateTelemetryEventParams{AppID: app, Kind: "crash", Message: "exited with 1"}); err != nil {
		t.Fatal(err)
	}
	issues := open()
	if len(issues) != 3 {
		t.Fatalf("%d issues, want the dumps, the other error and the crash: %+v", len(issues), issues)
	}
	var dumps teldb.ListIssuesRow
	for _, i := range issues {
		if i.Count == 3 {
			dumps = i
		}
	}
	if dumps.ID == 0 || dumps.ResolvedAt != "" {
		t.Fatalf("the dumps aren't one open issue of 3: %+v", issues)
	}

	// Any event of the issue resolves it, not just the latest.
	first, err := s.Tel.ListTelemetryEvents(ctx, teldb.ListTelemetryEventsParams{AppID: app, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	oldest := first[len(first)-1].ID
	if ok, err := tel.ResolveIssue(ctx, app, oldest); err != nil || !ok {
		t.Fatalf("resolve: %v, %v", ok, err)
	}
	if got := open(); len(got) != 2 {
		t.Errorf("%d open issues after resolving one, want 2", len(got))
	}
	all, err := tel.AppIssues(ctx, app, true, 50)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, i := range all {
		if i.ResolvedAt != "" {
			resolved++
		}
	}
	if len(all) != 3 || resolved != 1 {
		t.Errorf("with the resolved: %d issues, %d resolved; want 3, 1", len(all), resolved)
	}

	// Seen again: open again, counting every time it was seen.
	dump(3)
	reopened := open()
	if len(reopened) != 3 || reopened[0].Count != 4 || reopened[0].ResolvedAt != "" {
		t.Errorf("after the dump came back: %+v", reopened)
	}

	if ok, _ := tel.ResolveIssue(ctx, app, reopened[0].ID); !ok {
		t.Fatal("resolve again failed")
	}
	if ok, err := tel.ReopenIssue(ctx, app, reopened[0].ID); err != nil || !ok {
		t.Fatalf("reopen: %v, %v", ok, err)
	}
	if got := open(); len(got) != 3 {
		t.Errorf("%d open issues after reopening, want 3", len(got))
	}

	// Another app's event, or one that isn't there, resolves nothing.
	if ok, _ := tel.ResolveIssue(ctx, app+1, oldest); ok {
		t.Error("resolved another app's issue")
	}
	if ok, _ := tel.ResolveIssue(ctx, app, 9999); ok {
		t.Error("resolved an event that isn't there")
	}

	if err := tel.DropTelemetry(ctx, app); err != nil {
		t.Fatal(err)
	}
	if all, _ := tel.AppIssues(ctx, app, true, 50); len(all) != 0 {
		t.Errorf("%d issues kept after the app's telemetry was dropped", len(all))
	}
}
