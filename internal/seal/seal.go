// Package seal keeps a project's secrets in its repository, in
// hakobu.secrets.toml: each value encrypted on its own with age, to the
// recipients listed at the top of the file (the project's server, and the
// owner's recovery key). Anyone with the file can add or replace a secret,
// an agent included; only the server and the owner can read one. A diff
// shows which secret changed, never its value.
package seal

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"filippo.io/age"
	"github.com/BurntSushi/toml"
)

// FileName is where a project's secrets live, next to hakobu.toml.
const FileName = "hakobu.secrets.toml"

// prefix marks a sealed value, so a value pasted in plain text is caught.
const prefix = "age:"

// Hakobu's own secrets about a project, in the [hakobu] table.
const (
	// StatusToken may set commit statuses on the project's repository.
	StatusToken = "status_token"
)

var hakobuNames = []string{StatusToken}

// Secrets is a hakobu.secrets.toml.
type Secrets struct {
	// Recipients are age public keys (age1…); every value is sealed to
	// all of them.
	Recipients []string `toml:"recipients"`
	// Apps are each app's variables, sealed.
	Apps map[string]map[string]string `toml:"apps,omitempty"`
	// Hakobu are hakobu's own secrets of the project, sealed.
	Hakobu map[string]string `toml:"hakobu,omitempty"`
}

var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Parse reads a hakobu.secrets.toml strictly.
func Parse(data []byte) (*Secrets, error) {
	var s Secrets
	md, err := toml.Decode(string(data), &s)
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
	return &s, s.check()
}

func (s *Secrets) check() error {
	var errs []error
	if len(s.Recipients) == 0 {
		errs = append(errs, errors.New("recipients: none, so nobody could read the secrets"))
	}
	for _, r := range s.Recipients {
		if _, err := age.ParseX25519Recipient(r); err != nil {
			errs = append(errs, fmt.Errorf("recipients: %q isn't an age public key (age1…)", r))
		}
	}
	for _, app := range sorted(s.Apps) {
		for _, k := range sorted(s.Apps[app]) {
			if !envPattern.MatchString(k) {
				errs = append(errs, fmt.Errorf("apps.%s.%s: not a variable name (uppercase letters, digits and _)", app, k))
			}
			if !strings.HasPrefix(s.Apps[app][k], prefix) {
				errs = append(errs, fmt.Errorf("apps.%s.%s: not sealed; seal it with secret set, never by hand", app, k))
			}
		}
	}
	for _, k := range sorted(s.Hakobu) {
		if !slices.Contains(hakobuNames, k) {
			errs = append(errs, fmt.Errorf("hakobu.%s: hakobu has no such secret (it knows %s)", k, strings.Join(hakobuNames, ", ")))
		}
		if !strings.HasPrefix(s.Hakobu[k], prefix) {
			errs = append(errs, fmt.Errorf("hakobu.%s: not sealed; seal it with secret set, never by hand", k))
		}
	}
	return errors.Join(errs...)
}

// CheckApps reports secrets of apps that aren't among apps (hakobu.toml's),
// a typo the server would otherwise refuse the release for.
func (s *Secrets) CheckApps(apps []string) error {
	var errs []error
	for _, app := range sorted(s.Apps) {
		if !slices.Contains(apps, app) {
			errs = append(errs, fmt.Errorf("apps.%s: hakobu.toml has no such app (it has %s)", app, strings.Join(apps, ", ")))
		}
	}
	return errors.Join(errs...)
}

// New is an empty file for these recipients.
func New(recipients ...string) (*Secrets, error) {
	s := &Secrets{Recipients: recipients}
	return s, s.check()
}

// Set seals value as project's app's variable name; app "hakobu" is
// hakobu's own table.
func (s *Secrets) Set(project, app, name, value string) error {
	if len(value) > maxSecret {
		return fmt.Errorf("%s is more than 1 MiB: a secret that big goes in a volume, not the environment", name)
	}
	if project == "" || strings.ContainsAny(project+app, "/\n") {
		return fmt.Errorf("%q of %q: not a project and app to seal a secret for", app, project)
	}
	sealed, err := s.SealAt(Place(project, app, name), value)
	if err != nil {
		return err
	}
	if app == "hakobu" {
		if !slices.Contains(hakobuNames, name) {
			return fmt.Errorf("hakobu has no secret %s (it knows %s)", name, strings.Join(hakobuNames, ", "))
		}
		if s.Hakobu == nil {
			s.Hakobu = map[string]string{}
		}
		s.Hakobu[name] = sealed
		return nil
	}
	if !envPattern.MatchString(name) {
		return fmt.Errorf("%s isn't a variable name (uppercase letters, digits and _)", name)
	}
	if s.Apps == nil {
		s.Apps = map[string]map[string]string{}
	}
	if s.Apps[app] == nil {
		s.Apps[app] = map[string]string{}
	}
	s.Apps[app][name] = sealed
	return nil
}

// Unset removes a secret; it reports whether there was one.
func (s *Secrets) Unset(app, name string) bool {
	m := s.Apps[app]
	if app == "hakobu" {
		m = s.Hakobu
	}
	if _, ok := m[name]; !ok {
		return false
	}
	delete(m, name)
	if app != "hakobu" && len(m) == 0 {
		delete(s.Apps, app)
	}
	return true
}

// Seal seals value to the file's recipients, as a secret is: what CI
// hands the server in a release besides the file (hakobu.toml, its pull
// token) goes the same way.
func (s *Secrets) Seal(value string) (string, error) { return s.seal(value) }

// OpenValue opens one value Seal sealed.
func OpenValue(v string, id age.Identity) (string, error) {
	plain, err := unseal(v, id)
	if err == nil && len(plain) > maxSecret {
		return "", errors.New("a secret of more than 1 MiB")
	}
	return plain, err
}

// placed starts the text of a value sealed for its place, before the
// place, a newline and the value. The server's key opens the values of
// every project on it, and those of one are in its repository and its
// release, so one sealed bare could be pasted into another project's
// file, for its apps to read: one sealed for its place opens there alone.
const placed = "hakobu-secret/1 "

// Place is where a secret is sealed for: project, app ("hakobu" for
// hakobu's own) and variable.
func Place(project, app, name string) string { return project + "/" + app + "/" + name }

// SealAt seals value for place alone.
func (s *Secrets) SealAt(place, value string) (string, error) {
	return s.seal(placed + place + "\n" + value)
}

// OpenAt opens a value sealed for place, or sealed bare by a hakobu
// before 1.6 (bare is true then: it could be any project's). A value
// sealed for another place is an error.
func OpenAt(v string, id age.Identity, place string) (plain string, bare bool, err error) {
	plain, err = unseal(v, id)
	if err != nil {
		return "", false, err
	}
	if rest, ok := strings.CutPrefix(plain, placed); ok {
		at, value, _ := strings.Cut(rest, "\n")
		if at != place {
			return "", false, fmt.Errorf("sealed for %s, not %s: set it again here", at, place)
		}
		plain = value
	} else {
		bare = true
	}
	if len(plain) > maxSecret {
		return "", false, errors.New("a secret of more than 1 MiB")
	}
	return plain, bare, nil
}

func (s *Secrets) seal(value string) (string, error) {
	var rs []age.Recipient
	for _, r := range s.Recipients {
		rc, err := age.ParseX25519Recipient(r)
		if err != nil {
			return "", fmt.Errorf("recipients: %q: %w", r, err)
		}
		rs = append(rs, rc)
	}
	if len(rs) == 0 {
		return "", errors.New("no recipients to seal to")
	}
	var b bytes.Buffer
	w, err := age.Encrypt(&b, rs...)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, value); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(b.Bytes()), nil
}

// Opened are a file's secrets in the clear.
type Opened struct {
	Apps   map[string]map[string]string
	Hakobu map[string]string
	// Bare are the values sealed before hakobu sealed each for its
	// place, for the server to tell whether they're this project's.
	Bare []Bare
}

// Bare is a value sealed bare: its app ("hakobu" for hakobu's own),
// variable and sealed text.
type Bare struct{ App, Name, Sealed string }

// Open unseals every value with id, as project's; a value it can't open,
// or sealed for another place, is an error naming it, so a server left
// out of the recipients says so.
func (s *Secrets) Open(id age.Identity, project string) (Opened, error) {
	o := Opened{Apps: map[string]map[string]string{}, Hakobu: map[string]string{}}
	var errs []error
	open := func(at, app, name, v string) string {
		plain, bare, err := OpenAt(v, id, Place(project, app, name))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
		}
		if bare {
			o.Bare = append(o.Bare, Bare{app, name, v})
		}
		return plain
	}
	for _, app := range sorted(s.Apps) {
		o.Apps[app] = map[string]string{}
		for _, k := range sorted(s.Apps[app]) {
			o.Apps[app][k] = open("apps."+app+"."+k, app, k, s.Apps[app][k])
		}
	}
	for _, k := range sorted(s.Hakobu) {
		o.Hakobu[k] = open("hakobu."+k, "hakobu", k, s.Hakobu[k])
	}
	return o, errors.Join(errs...)
}

// ErrNotARecipient: the value wasn't sealed to the key opening it.
var ErrNotARecipient = errors.New("not sealed to this key; add its public key to recipients and set the secret again")

func unseal(v string, id age.Identity) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	if err != nil {
		return "", errors.New("damaged: not base64")
	}
	r, err := age.Decrypt(bytes.NewReader(b), id)
	var none *age.NoIdentityMatchError
	if errors.As(err, &none) {
		return "", ErrNotARecipient
	}
	if err != nil {
		return "", err
	}
	// Room for the place too; the callers hold the value to maxSecret.
	plain, err := io.ReadAll(io.LimitReader(r, maxSecret+4<<10+1))
	if err == nil && len(plain) > maxSecret+4<<10 {
		return "", errors.New("a secret of more than 1 MiB")
	}
	return string(plain), err
}

// maxSecret is the most a secret holds.
const maxSecret = 1 << 20

// Marshal writes the file back, keys in order, recipients first.
func (s *Secrets) Marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Sealed with age to the recipients below: change with hakobu secret set.\n")
	err := toml.NewEncoder(&b).Encode(s)
	return b.Bytes(), err
}

// NewIdentity makes a key pair: the identity (AGE-SECRET-KEY-1…) to keep
// private and its recipient (age1…) to share.
func NewIdentity() (*age.X25519Identity, error) { return age.GenerateX25519Identity() }

// LoadIdentity reads an identity file, as age-keygen writes one.
func LoadIdentity(file string) (*age.X25519Identity, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			return age.ParseX25519Identity(line)
		}
	}
	return nil, fmt.Errorf("%s: no age identity in it", file)
}

// SaveIdentity writes id to file, readable by its owner alone, refusing to
// replace one: a lost key loses every secret and backup sealed to it.
func SaveIdentity(file string, id *age.X25519Identity) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "# public key: %s\n%s\n", id.Recipient(), id)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func sorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
