package ops

import (
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestReferences(t *testing.T) {
	s := notifyStore(t)
	for _, p := range []string{"shop", "other"} {
		if err := CreateProject(s, 0, p); err != nil {
			t.Fatal(err)
		}
	}
	shop, _ := s.GetProject(ctx(), "shop")
	other, _ := s.GetProject(ctx(), "other")
	for name, project := range map[string]int64{"web": shop.ID, "api": shop.ID, "jobs": shop.ID, "elsewhere": other.ID} {
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: project, Name: name, BuildStrategy: "railpack"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{Name: "api", Domain: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{Name: "api", ContainerPort: 8080}); err != nil {
		t.Fatal(err)
	}
	web, _ := s.GetApp(ctx(), "web")

	got, err := expandRefs(s, web, []string{"A=${{api.URL}}/v1", "B=${{ api.HOST }}", "C=${{api.PRIVATE_URL}}", "D=plain $HOME"})
	if want := "A=https://api.example.com/v1 B=api.example.com C=http://api.hakobu:8080 D=plain $HOME"; err != nil || strings.Join(got, " ") != want {
		t.Errorf("expanded %q (%v), want %s", got, err, want)
	}
	for ref, why := range map[string]string{
		"${{elsewhere.URL}}":    "names no app of project shop",
		"${{nobody.URL}}":       "names no app of project shop",
		"${{jobs.URL}}":         "jobs is private",
		"${{jobs.PRIVATE_URL}}": "deploy jobs first",
	} {
		if _, err := expandRefs(s, web, []string{"X=" + ref}); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: %v, want %q", ref, err, why)
		}
	}

	if !RefersTo("API=${{ api.URL }}", "api") || RefersTo("API=${{api.URL}}", "ap") || RefersTo("API=https://api.example.com", "api") {
		t.Error("RefersTo")
	}
}
