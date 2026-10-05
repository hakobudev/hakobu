package ops

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/x0ryz/hakobu/internal/store"
)

// References, like Railway's: a variable's value can name another app of
// the same project, ${{api.URL}}, and gets what it is when the app
// deploys. The project's canvas draws an arrow for each.
//
//	${{api.URL}}          https://api.example.com, its public address
//	${{api.HOST}}         api.example.com
//	${{api.PRIVATE_URL}}  http://api.hakobu:8080, inside the project, past
//	                      Cloudflare: for a server, not a browser
var refPattern = regexp.MustCompile(`\$\{\{\s*([a-z][a-z0-9-]*)\.(URL|HOST|PRIVATE_URL)\s*\}\}`)

// expandRefs fills in the references in env's values, to apps of app's
// project; one it can't fill in fails the deploy, saying why.
func expandRefs(s *store.Store, app store.App, env []string) ([]string, error) {
	out := make([]string, len(env))
	for i, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		var err error
		value = refPattern.ReplaceAllStringFunc(value, func(ref string) string {
			m := refPattern.FindStringSubmatch(ref)
			v, rerr := refValue(s, app, m[1], m[2])
			if rerr != nil && err == nil {
				err = fmt.Errorf("%s: %s %w", key, ref, rerr)
			}
			return v
		})
		if err != nil {
			return nil, err
		}
		out[i] = key + "=" + value
	}
	return out, nil
}

func refValue(s *store.Store, app store.App, name, field string) (string, error) {
	target, err := s.GetApp(ctx(), name)
	if err != nil || target.ProjectID != app.ProjectID {
		return "", fmt.Errorf("names no app of project %s", app.ProjectName)
	}
	switch field {
	case "URL", "HOST":
		if target.Domain == "" {
			return "", fmt.Errorf("is empty: %s is private; give it an address, or use PRIVATE_URL from a server", name)
		}
		if field == "HOST" {
			return target.Domain, nil
		}
		return PublicURL(target), nil
	}
	port := target.ContainerPort
	if port == 0 {
		port = target.LivePort
	}
	if port == 0 {
		return "", fmt.Errorf("has no port yet: deploy %s first, or set its port", name)
	}
	return fmt.Sprintf("http://%s:%d", EdgeAlias(name), port), nil
}

// RefersTo reports whether the variables in text refer to app.
func RefersTo(text, app string) bool {
	for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
		if m[1] == app {
			return true
		}
	}
	return false
}
