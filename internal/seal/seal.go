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

// Set seals value as app's variable name; app "hakobu" is hakobu's own
// table.
func (s *Secrets) Set(app, name, value string) error {
	if len(value) > maxSecret {
		return fmt.Errorf("%s is more than 1 MiB: a secret that big goes in a volume, not the environment", name)
	}
	sealed, err := s.seal(value)
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
func OpenValue(v string, id age.Identity) (string, error) { return unseal(v, id) }

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
}

// Open unseals every value with id; a value it can't open is an error
// naming it, so a server left out of the recipients says so.
func (s *Secrets) Open(id age.Identity) (Opened, error) {
	o := Opened{Apps: map[string]map[string]string{}, Hakobu: map[string]string{}}
	var errs []error
	open := func(at, v string) string {
		plain, err := unseal(v, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
		}
		return plain
	}
	for _, app := range sorted(s.Apps) {
		o.Apps[app] = map[string]string{}
		for _, k := range sorted(s.Apps[app]) {
			o.Apps[app][k] = open("apps."+app+"."+k, s.Apps[app][k])
		}
	}
	for _, k := range sorted(s.Hakobu) {
		o.Hakobu[k] = open("hakobu."+k, s.Hakobu[k])
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
	plain, err := io.ReadAll(io.LimitReader(r, maxSecret+1))
	if err == nil && len(plain) > maxSecret {
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
