package spec

import (
	"strings"
	"testing"
)

const kasl = `
project = "kasl"
server  = "kasl-1"

[apps.crm]
domain  = "crm.kaslauto.com"
release = "bun run migrate"
worker  = "bun run worker"
memory  = "1gb"
uses    = ["main", "cache"]
volumes = { uploads = "/app/data" }
env     = { NODE_ENV = "production" }

[databases.main]

[caches.cache]
`

func TestParse(t *testing.T) {
	p, err := Parse([]byte(kasl))
	if err != nil {
		t.Fatal(err)
	}
	crm := p.Apps["crm"]
	if p.Name != "kasl" || p.Server != "kasl-1" || crm.Domain != "crm.kaslauto.com" {
		t.Errorf("read %+v", p)
	}
	if crm.Health != DefaultHealth {
		t.Errorf("health = %q, want the default", crm.Health)
	}
	if crm.Memory != 1<<30 || p.Caches["cache"].Memory != DefaultCacheMemory {
		t.Errorf("memory = %v, cache %v", crm.Memory, p.Caches["cache"].Memory)
	}
	if len(crm.Uses) != 2 || crm.Uses["main"] != "" || crm.Uses["cache"] != "" {
		t.Errorf("uses = %v", crm.Uses)
	}
	if crm.Volumes["uploads"] != "/app/data" || crm.Env["NODE_ENV"] != "production" {
		t.Errorf("volumes %v, env %v", crm.Volumes, crm.Env)
	}
	if _, ok := p.Databases["main"]; !ok {
		t.Error("no database main")
	}
}

func TestUsesPrefixes(t *testing.T) {
	p, err := Parse([]byte(`
project = "p"
server = "s"
[apps.web]
uses = { main = "", stats = "STATS" }
[databases.main]
[databases.stats]
`))
	if err != nil {
		t.Fatal(err)
	}
	if u := p.Apps["web"].Uses; u["main"] != "" || u["stats"] != "STATS" {
		t.Errorf("uses = %v", u)
	}
	if v := Vars("database", "STATS")[0]; v != "STATS_DATABASE_URL" {
		t.Errorf("Vars = %s", v)
	}
}

func TestReadyImage(t *testing.T) {
	p, err := Parse([]byte(`project = "p"
server = "s"
[apps.wa]
image = "evoapicloud/evolution-api:v2.3.7"
uses = ["gateway"]
env = { DATABASE_CONNECTION_URI = "${DATABASE_URL}?schema=evolution", SERVER_URL = "http://wa:${PORT}" }
[databases.gateway]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Expand(p.Apps["wa"].Env["DATABASE_CONNECTION_URI"], map[string]string{"DATABASE_URL": "postgres://x"}); got != "postgres://x?schema=evolution" {
		t.Errorf("Expand = %q", got)
	}
	for ref, tag := range map[string]string{"a/b:1.2": "1.2", "host:5000/a/b": "", "a/b@sha256:00": "sha256:00", "b": ""} {
		if got := ImageTag(ref); got != tag {
			t.Errorf("ImageTag(%q) = %q, want %q", ref, got, tag)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	head := "project = \"p\"\nserver = \"s\"\n"
	for _, c := range []struct{ name, toml, want string }{
		{"typo", head + "[apps.web]\nhelth = \"/up\"", "unknown keys: apps.web.helth"},
		{"top typo", head + "servr = \"x\"\n[apps.web]", "unknown keys: servr"},
		{"no apps", head, "at least one"},
		{"no project", "server = \"s\"\n[apps.web]", "project:"},
		{"bad name", head + "[apps.Web]", "apps.Web: not a name"},
		{"uses nothing", head + "[apps.web]\nuses = [\"db\"]", "no database or cache is named db"},
		{"two databases", head + "[apps.web]\nuses = [\"a\", \"b\"]\n[databases.a]\n[databases.b]", "would both set DATABASE_URL"},
		{"bad prefix", head + "[apps.web]\nuses = { a = \"low\" }\n[databases.a]", "prefix \"low\""},
		{"env clash", head + "[apps.web]\nuses = [\"a\"]\nenv = { DATABASE_URL = \"x\" }\n[databases.a]", "uses a sets it already"},
		{"env PORT", head + "[apps.web]\nenv = { PORT = \"80\" }", "set port instead"},
		{"image and path", head + "[apps.wa]\npath = \"wa\"\nimage = \"evo/api:2.3.7\"", "path or image, not both"},
		{"image untagged", head + "[apps.wa]\nimage = \"evo/api\"", "should name its version"},
		{"image latest", head + "[apps.wa]\nimage = \"registry:5000/evo/api:latest\"", "should name its version"},
		{"env names nothing", head + "[apps.web]\nenv = { DB = \"${DATABASE_URL}\" }", "${DATABASE_URL} isn't a variable hakobu gives"},
		{"env HAKOBU_REVISION", head + "[apps.web]\nenv = { HAKOBU_REVISION = \"x\" }", "hakobu sets it"},
		{"env name", head + "[apps.web]\nenv = { \"node-env\" = \"x\" }", "not a variable name"},
		{"same name", head + "[apps.web]\n[databases.x]\n[caches.x]", "a database has that name"},
		{"domain", head + "[apps.web]\ndomain = \"https://x.com\"", "isn't a domain name"},
		{"domain twice", head + "[apps.a]\ndomain = \"x.com\"\n[apps.b]\ndomain = \"x.com\"", "apps.a has x.com too"},
		{"health", head + "[apps.web]\nhealth = \"up\"", "should start with /"},
		{"memory", head + "[apps.web]\nmemory = \"lots\"", "isn't a size"},
		{"small memory", head + "[apps.web]\nmemory = \"32mb\"", "less than 64mb"},
		{"cache memory", head + "[apps.web]\n[caches.c]\nmemory = \"8mb\"", "less than 16mb"},
		{"volume path", head + "[apps.web]\nvolumes = { data = \"data\" }", "absolute path"},
		{"volume root", head + "[apps.web]\nvolumes = { data = \"/\" }", "absolute path"},
		{"volume twice", head + "[apps.web]\nvolumes = { a = \"/d\", b = \"/d/\" }", "mounted at"},
		{"path out", head + "[apps.web]\npath = \"../other\"", "inside the repository"},
		{"uses type", head + "[apps.web]\nuses = 3", "a list of names"},
		{"uses dup", head + "[apps.web]\nuses = [\"a\", \"a\"]\n[databases.a]", "listed twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.toml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestParseReportsEverything(t *testing.T) {
	_, err := Parse([]byte("project = \"P\"\nserver = \"S\"\n[apps.web]\nhealth = \"x\""))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"project:", "server:", "health"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}

func TestSize(t *testing.T) {
	for in, want := range map[string]Size{"256mb": 256 << 20, "1gb": 1 << 30, "1.5GB": 3 << 29, " 64 MB ": 64 << 20} {
		var s Size
		if err := s.UnmarshalText([]byte(in)); err != nil || s != want {
			t.Errorf("%q = %v, %v; want %v", in, s, err, want)
		}
	}
	for _, in := range []string{"256", "mb", "-1mb", "1tb", "0.1mb"} {
		var s Size
		if err := s.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("%q = %v, want an error", in, s)
		}
	}
	if got := Size(1 << 30).String(); got != "1gb" {
		t.Errorf("String = %s", got)
	}
	for _, s := range []Size{0, 300 << 20, 2 << 30} {
		b, _ := s.MarshalText()
		var back Size
		if err := back.UnmarshalText(b); err != nil || back != s {
			t.Errorf("%v → %q → %v, %v", s, b, back, err)
		}
	}
}
