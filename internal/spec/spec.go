// Package spec reads hakobu.toml, what a project's server should run. It's
// strict: an unknown key or a reference to nothing is an error, so a typo
// stops a deploy instead of quietly turning something off.
package spec

import (
	"errors"
	"fmt"
	"math"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// File is the name of a project's spec, at the root of its repository.
const File = "hakobu.toml"

// RevisionVar is the variable each app gets the commit it was deployed from in, so it can tell
// which version it is (an error tracker's release, a footer).
const RevisionVar = "HAKOBU_REVISION"

// Project is a repository's hakobu.toml.
type Project struct {
	Name      string              `toml:"project"`
	Server    string              `toml:"server"`
	Apps      map[string]App      `toml:"apps"`
	Databases map[string]Database `toml:"databases"`
	Caches    map[string]Cache    `toml:"caches"`
}

// App is a process serving HTTP, with an optional worker from the same
// image.
type App struct {
	// Path is the app's directory in a monorepo, for the build.
	Path string `toml:"path"`
	// Image is a ready image to run instead of building one, a service
	// such as evoapicloud/evolution-api:v2.3.7. The build pins it to the
	// digest its tag names then.
	Image string `toml:"image"`
	// Domain is where it's served; empty for an app only other apps of
	// the project reach.
	Domain string `toml:"domain"`
	// Health is the path answered 2xx once the app can take requests.
	Health string `toml:"health"`
	// Port is the one it listens on; 0 takes the image's EXPOSE, or PORT.
	Port int `toml:"port"`
	// Release runs once in the new image before it takes requests, for
	// migrations: if it fails, the running version stays.
	Release string `toml:"release"`
	// Worker is the command of a background process from the same image.
	Worker  string            `toml:"worker"`
	Memory  Size              `toml:"memory"`
	CPUs    float64           `toml:"cpus"`
	Volumes map[string]string `toml:"volumes"` // name → mount path
	Env     map[string]string `toml:"env"`
	Uses    Uses              `toml:"uses"`
}

// Database is a database and its user on the server's PostgreSQL, whose
// version is the server's.
type Database struct{}

// Cache is a Valkey server of the project's own.
type Cache struct {
	Memory Size `toml:"memory"`
}

// Uses maps the databases and caches an app connects to onto the prefix of
// their variables: "" gives DATABASE_URL, "STATS" gives STATS_DATABASE_URL.
// In the file it's a list of names (no prefixes) or a table of them.
type Uses map[string]string

func (u *Uses) UnmarshalTOML(v any) error {
	*u = Uses{}
	switch v := v.(type) {
	case []any:
		for _, n := range v {
			s, ok := n.(string)
			if !ok {
				return fmt.Errorf("uses: %v isn't a name", n)
			}
			if _, dup := (*u)[s]; dup {
				return fmt.Errorf("uses: %s is listed twice", s)
			}
			(*u)[s] = ""
		}
	case map[string]any:
		for n, p := range v {
			s, ok := p.(string)
			if !ok {
				return fmt.Errorf("uses: the prefix of %s isn't a string", n)
			}
			(*u)[n] = s
		}
	default:
		return errors.New(`uses: a list of names, or a table of name = "PREFIX"`)
	}
	return nil
}

// Size is an amount of memory, written like "256mb" or "1gb", in whole
// megabytes; "" is none.
type Size int64

const mb = 1 << 20

func (s *Size) UnmarshalText(b []byte) error {
	t := strings.ToLower(strings.TrimSpace(string(b)))
	if t == "" {
		*s = 0
		return nil
	}
	unit := int64(0)
	for suffix, n := range map[string]int64{"mb": mb, "gb": 1 << 30} {
		if strings.HasSuffix(t, suffix) {
			t, unit = strings.TrimSuffix(t, suffix), n
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if unit == 0 || err != nil || n <= 0 {
		return fmt.Errorf("%q isn't a size like 256mb or 1gb", b)
	}
	*s = Size(math.Round(n*float64(unit)/mb)) * mb
	if *s == 0 {
		return fmt.Errorf("%q is less than 1mb", b)
	}
	return nil
}

func (s Size) MarshalText() ([]byte, error) {
	if s == 0 {
		return nil, nil
	}
	return []byte(s.String()), nil
}

func (s Size) String() string {
	if s%(1<<30) == 0 {
		return fmt.Sprintf("%dgb", s>>30)
	}
	return fmt.Sprintf("%dmb", s/mb)
}

const (
	DefaultHealth      = "/up"
	DefaultCacheMemory = 256 * mb
)

// Parse reads a hakobu.toml, fills in defaults and checks it, reporting
// every problem at once.
func Parse(data []byte) (*Project, error) {
	var p Project
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return nil, err
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(names, ", "))
	}
	p.defaults()
	if err := p.check(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Project) defaults() {
	for name, a := range p.Apps {
		if a.Health == "" {
			a.Health = DefaultHealth
		}
		p.Apps[name] = a
	}
	for name, c := range p.Caches {
		if c.Memory == 0 {
			c.Memory = DefaultCacheMemory
		}
		p.Caches[name] = c
	}
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	envPattern  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// refPattern is ${NAME} in a variable's value: another variable hakobu
	// gives the app, for an image that wants it under its own name.
	refPattern    = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)
	prefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	labelPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const nameRule = "lowercase letters, digits and -, starting with a letter"

func (p *Project) check() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !namePattern.MatchString(p.Name) {
		bad("project: %q isn't a name (%s)", p.Name, nameRule)
	}
	if !namePattern.MatchString(p.Server) {
		bad("server: %q isn't a name (%s)", p.Server, nameRule)
	}
	if len(p.Apps) == 0 {
		bad("apps: a project needs at least one")
	}
	kinds := map[string]string{} // database or cache name → its kind
	for _, n := range sorted(p.Databases) {
		if !namePattern.MatchString(n) {
			bad("databases.%s: not a name (%s)", n, nameRule)
		}
		kinds[n] = "database"
	}
	for _, n := range sorted(p.Caches) {
		if !namePattern.MatchString(n) {
			bad("caches.%s: not a name (%s)", n, nameRule)
		}
		if _, dup := kinds[n]; dup {
			bad("caches.%s: a database has that name; uses couldn't tell them apart", n)
		}
		kinds[n] = "cache"
		if m := p.Caches[n].Memory; m < 16*mb {
			bad("caches.%s.memory: %s is less than 16mb", n, m)
		}
	}
	domains := map[string]string{}
	for _, n := range sorted(p.Apps) {
		a := p.Apps[n]
		at := "apps." + n
		if !namePattern.MatchString(n) {
			bad("%s: not a name (%s)", at, nameRule)
		}
		if a.Domain != "" {
			if !isDomain(a.Domain) {
				bad("%s.domain: %q isn't a domain name", at, a.Domain)
			} else if other, dup := domains[a.Domain]; dup {
				bad("%s.domain: apps.%s has %s too", at, other, a.Domain)
			}
			domains[a.Domain] = n
		}
		if !strings.HasPrefix(a.Health, "/") {
			bad("%s.health: %q should start with /", at, a.Health)
		}
		if a.Port < 0 || a.Port > 65535 {
			bad("%s.port: %d isn't a port", at, a.Port)
		}
		if a.Memory != 0 && a.Memory < 64*mb {
			bad("%s.memory: %s is less than 64mb", at, a.Memory)
		}
		if a.CPUs < 0 {
			bad("%s.cpus: can't be negative", at)
		}
		if a.Path != "" && (path.IsAbs(a.Path) || strings.HasPrefix(path.Clean(a.Path), "..")) {
			bad("%s.path: %q should be a directory inside the repository", at, a.Path)
		}
		if a.Image != "" {
			if a.Path != "" {
				bad("%s: path or image, not both: a path is built, an image is run as it is", at)
			}
			if tag := ImageTag(a.Image); tag == "" || tag == "latest" {
				bad("%s.image: %q should name its version, as in name:1.2.3 (a deploy pins what the tag names at build time)", at, a.Image)
			}
		}
		mounts := map[string]string{}
		for _, v := range sorted(a.Volumes) {
			mount := a.Volumes[v]
			if !namePattern.MatchString(v) {
				bad("%s.volumes.%s: not a name (%s)", at, v, nameRule)
			}
			if !path.IsAbs(mount) || path.Clean(mount) == "/" {
				bad("%s.volumes.%s: %q should be an absolute path below /", at, v, mount)
			} else if other, dup := mounts[path.Clean(mount)]; dup {
				bad("%s.volumes.%s: volume %s is mounted at %s too", at, v, other, mount)
			}
			mounts[path.Clean(mount)] = v
		}
		given := map[string]string{} // variable → what gives it
		for _, r := range sorted(a.Uses) {
			prefix := a.Uses[r]
			kind, ok := kinds[r]
			if !ok {
				bad("%s.uses: no database or cache is named %s", at, r)
				continue
			}
			if prefix != "" && !prefixPattern.MatchString(prefix) {
				bad("%s.uses.%s: prefix %q should be uppercase letters, digits and _", at, r, prefix)
				continue
			}
			for _, v := range Vars(kind, prefix) {
				if other, dup := given[v]; dup {
					bad("%s.uses: %s and %s would both set %s; give one a prefix", at, other, r, v)
					break
				}
				given[v] = r
			}
		}
		for _, k := range sorted(a.Env) {
			for _, m := range refPattern.FindAllStringSubmatch(a.Env[k], -1) {
				if _, ok := given[m[1]]; !ok && m[1] != "PORT" && m[1] != RevisionVar {
					bad("%s.env.%s: ${%s} isn't a variable hakobu gives the app (from uses, PORT or %s)", at, k, m[1], RevisionVar)
				}
			}
			if !envPattern.MatchString(k) {
				bad("%s.env.%s: not a variable name (uppercase letters, digits and _)", at, k)
			}
			if k == "PORT" {
				bad("%s.env.PORT: set port instead", at)
			}
			if k == RevisionVar {
				bad("%s.env.%s: hakobu sets it, to the commit deployed", at, k)
			}
			if r, dup := given[k]; dup {
				bad("%s.env.%s: uses %s sets it already", at, k, r)
			}
		}
	}
	return errors.Join(errs...)
}

// ImageTag is the tag of an image reference, "" if it has none (a digest
// counts as one).
func ImageTag(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return digest
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	if _, tag, ok := strings.Cut(name, ":"); ok {
		return tag
	}
	return ""
}

// Expand replaces each ${NAME} in value with vars[NAME].
func Expand(value string, vars map[string]string) string {
	return refPattern.ReplaceAllStringFunc(value, func(m string) string {
		return vars[refPattern.FindStringSubmatch(m)[1]]
	})
}

// Vars are the variables a database or cache gives an app that uses it,
// as hakobu has always named them.
func Vars(kind, prefix string) []string {
	names := map[string][]string{
		"database": {"DATABASE_URL", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD"},
		"cache":    {"REDIS_URL", "VALKEY_URL", "REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD"},
	}[kind]
	if prefix == "" {
		return names
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + "_" + n
	}
	return out
}

func isDomain(s string) bool {
	if net.ParseIP(s) != nil || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !labelPattern.MatchString(l) {
			return false
		}
	}
	return true
}

func sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
