package ops

import (
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/store"
)

func TestReferences(t *testing.T) {
	s := notifyStore(t)
	for _, p := range []string{"shop", "other"} {
		if _, err := CreateProject(s, 0, p); err != nil {
			t.Fatal(err)
		}
	}
	shop, _ := getProject(s, "shop")
	other, _ := getProject(s, "other")
	for name, project := range map[string]int64{"web": shop.ID, "api": shop.ID, "jobs": shop.ID, "elsewhere": other.ID} {
		if err := createApp(s, store.CreateAppParams{ProjectID: project, Name: name, BuildStrategy: "railpack"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{ID: appID(t, s, "api"), Domain: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{ID: appID(t, s, "api"), ContainerPort: 8080}); err != nil {
		t.Fatal(err)
	}
	web, _ := getApp(s, "web")

	got, err := expandRefs(s, web, []string{"A=${{api.URL}}/v1", "B=${{ api.HOST }}", "C=${{api.PRIVATE_URL}}", "D=plain $HOME"})
	if want := "A=https://api.example.com/v1 B=api.example.com C=http://"+EdgeAlias(mustApp(t, s, "api").Key())+":8080 D=plain $HOME"; err != nil || strings.Join(got, " ") != want {
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
