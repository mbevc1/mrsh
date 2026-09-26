// Package config parses, validates and merges hosts.yaml, and resolves secret references.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultPort is the SSH port used when neither the host nor defaults set one.
const DefaultPort = 22

// Host key policies accepted by host_key_policy / --host-key-policy.
const (
	HostKeyStrict    = "strict"
	HostKeyAcceptNew = "accept-new"
	HostKeyInsecure  = "insecure"
)

// ErrSOPSEncrypted is returned for a config carrying a top-level sops block.
var ErrSOPSEncrypted = errors.New("config appears to be SOPS-encrypted; encrypted config is not supported in this version")

// Config is the parsed hosts.yaml.
type Config struct {
	Defaults Defaults `yaml:"defaults,omitempty" json:"defaults"`
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`
	Hosts    []Host   `yaml:"hosts" json:"hosts"`
}

// Credentials holds the user and pass field-groups shared by Defaults and Host.
// Exactly one variant of each group may be set.
type Credentials struct {
	User    string `yaml:"user,omitempty" json:"user,omitempty"`
	UserEnv string `yaml:"user_env,omitempty" json:"user_env,omitempty"`
	UserARN string `yaml:"user_arn,omitempty" json:"user_arn,omitempty"`
	Pass    string `yaml:"pass,omitempty" json:"pass,omitempty"`
	PassEnv string `yaml:"pass_env,omitempty" json:"pass_env,omitempty"`
	PassARN string `yaml:"pass_arn,omitempty" json:"pass_arn,omitempty"`
}

// Defaults apply to every host that does not override them.
type Defaults struct {
	Credentials   `yaml:",inline"`
	Port          int    `yaml:"port,omitempty" json:"port,omitempty"`
	Timeout       int    `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	IdentityFile  string `yaml:"identity_file,omitempty" json:"identity_file,omitempty"`
	KnownHosts    string `yaml:"known_hosts,omitempty" json:"known_hosts,omitempty"`
	HostKeyPolicy string `yaml:"host_key_policy,omitempty" json:"host_key_policy,omitempty"`
	Parallel      int    `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	Output        string `yaml:"output,omitempty" json:"output,omitempty"`
	Debug         bool   `yaml:"debug,omitempty" json:"debug,omitempty"`
}

// Host is one inventory entry.
type Host struct {
	Name         string `yaml:"name" json:"name"`
	Host         string `yaml:"host" json:"host"`
	Group        string `yaml:"group,omitempty" json:"group,omitempty"`
	Port         int    `yaml:"port,omitempty" json:"port,omitempty"`
	Credentials  `yaml:",inline"`
	IdentityFile string `yaml:"identity_file,omitempty" json:"identity_file,omitempty"`
	KnownHosts   string `yaml:"known_hosts,omitempty" json:"known_hosts,omitempty"`
}

// Parse decodes raw YAML strictly: unknown keys are errors, and a
// SOPS-encrypted file is refused before any decoding into Config.
func Parse(raw []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) == 0 {
		return &Config{}, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content); i += 2 {
			switch root.Content[i].Value {
			case "sops":
				return nil, ErrSOPSEncrypted
			case "mts":
				return nil, errors.New("parse config: key \"mts\" is from react; rename it to \"hosts\"")
			}
		}
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}

var validOutputs = map[string]bool{"": true, "text": true, "json": true, "csv": true}

var validHostKeyPolicies = map[string]bool{"": true, HostKeyStrict: true, HostKeyAcceptNew: true, HostKeyInsecure: true}

// Validate reports every problem in c, each naming the host and field.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	d := c.Defaults
	for _, e := range d.validate() {
		add("defaults: %v", e)
	}
	if !validPort(d.Port) {
		add("defaults: port %d out of range 1-65535", d.Port)
	}
	if d.Timeout < 0 {
		add("defaults: timeout must not be negative")
	}
	if d.Parallel < 0 {
		add("defaults: parallel must not be negative")
	}
	if !validOutputs[d.Output] {
		add("defaults: output %q must be text, json or csv", d.Output)
	}
	if !validHostKeyPolicies[d.HostKeyPolicy] {
		add("defaults: host_key_policy %q must be strict, accept-new or insecure", d.HostKeyPolicy)
	}

	seen := make(map[string]int, len(c.Hosts))
	for i, h := range c.Hosts {
		label := h.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		label = "host " + label

		switch {
		case h.Name == "":
			add("%s: name is required", label)
		case strings.ContainsAny(h.Name, " \t/"):
			add("%s: name must not contain whitespace or '/'", label)
		default:
			if prev, dup := seen[h.Name]; dup {
				add("%s: duplicate name (also host #%d)", label, prev)
			} else {
				seen[h.Name] = i + 1
			}
		}
		if err := validateAddress(h.Host); err != nil {
			add("%s: host %v", label, err)
		}
		if !validPort(h.Port) {
			add("%s: port %d out of range 1-65535", label, h.Port)
		}
		for _, e := range h.validate() {
			add("%s: %v", label, e)
		}
	}
	return errors.Join(errs...)
}

// validPort accepts 0 (unset) or a TCP port.
func validPort(p int) bool { return p >= 0 && p <= 65535 }

func validateAddress(a string) error {
	switch {
	case a == "":
		return errors.New("is required")
	case strings.ContainsAny(a, " \t\r\n"):
		return fmt.Errorf("%q must not contain whitespace", a)
	case strings.Contains(a, "://"):
		return fmt.Errorf("%q must be a hostname or IP, not a URL", a)
	case strings.Contains(a, "@"):
		return fmt.Errorf("%q must not include a user; set user instead", a)
	}
	return nil
}

func (c Credentials) validate() []error {
	var errs []error
	for _, ref := range []SecretRef{c.UserRef(), c.PassRef()} {
		if n := ref.variants(); n > 1 {
			errs = append(errs, fmt.Errorf("%s: set only one of %s, %s_env, %s_arn", ref.Field, ref.Field, ref.Field, ref.Field))
		}
		if ref.ARN != "" {
			if _, err := parseARN(ref.ARN); err != nil {
				errs = append(errs, fmt.Errorf("%s_arn: %v", ref.Field, err))
			}
		}
	}
	return errs
}

// UserRef returns the user field-group.
func (c Credentials) UserRef() SecretRef {
	return SecretRef{Field: "user", Literal: c.User, Env: c.UserEnv, ARN: c.UserARN}
}

// PassRef returns the pass field-group.
func (c Credentials) PassRef() SecretRef {
	return SecretRef{Field: "pass", Literal: c.Pass, Env: c.PassEnv, ARN: c.PassARN}
}

// Effective returns h with defaults applied. A host that sets any variant of
// a field-group (user*, pass*) replaces the defaults' whole group.
func (c *Config) Effective(h Host) Host {
	d := c.Defaults
	if h.Port == 0 {
		h.Port = d.Port
	}
	if h.Port == 0 {
		h.Port = DefaultPort
	}
	if h.UserRef().Kind() == KindUnset {
		h.User, h.UserEnv, h.UserARN = d.User, d.UserEnv, d.UserARN
	}
	if h.PassRef().Kind() == KindUnset {
		h.Pass, h.PassEnv, h.PassARN = d.Pass, d.PassEnv, d.PassARN
	}
	if h.IdentityFile == "" {
		h.IdentityFile = d.IdentityFile
	}
	if h.KnownHosts == "" {
		h.KnownHosts = d.KnownHosts
	}
	h.IdentityFile = ExpandHome(h.IdentityFile)
	h.KnownHosts = ExpandHome(h.KnownHosts)
	return h
}

// ExpandHome replaces a leading "~/" with the user's home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// Secret reference kinds, named after the debug-log backend labels.
const (
	KindUnset = "unset"
	KindPlain = "plain"
	KindEnv   = "env"
	KindARN   = "arn"
)

// SecretRef is one field-group (user or pass) with its three variants.
type SecretRef struct {
	Field   string // "user" or "pass"
	Literal string
	Env     string
	ARN     string
}

func (r SecretRef) variants() int {
	n := 0
	for _, v := range []string{r.Literal, r.Env, r.ARN} {
		if v != "" {
			n++
		}
	}
	return n
}

// Kind reports which variant is set. Validation guarantees at most one.
func (r SecretRef) Kind() string {
	switch {
	case r.Literal != "":
		return KindPlain
	case r.Env != "":
		return KindEnv
	case r.ARN != "":
		return KindARN
	}
	return KindUnset
}

// Mask replaces a literal secret in listings.
const Mask = "********"

// Display renders the reference for listings: literals are masked unless
// showSecrets is set; env/ARN references are not secrets and show as-is.
func (r SecretRef) Display(showSecrets bool) string {
	switch r.Kind() {
	case KindPlain:
		if showSecrets {
			return r.Literal
		}
		return Mask
	case KindEnv:
		return "env:" + r.Env
	case KindARN:
		return r.ARN
	}
	return ""
}

// arn is the parsed form of a supported *_arn value.
type arn struct {
	Service string // "ssm" or "secretsmanager"
	Region  string
	Base    string // ARN without any #field suffix
	Field   string // Secrets Manager JSON key, or ""
}

var arnRE = regexp.MustCompile(`^arn:aws[a-z-]*:(ssm|secretsmanager):([a-z0-9-]+):(\d{12}):(.+)$`)

func parseARN(s string) (arn, error) {
	base, field, hasField := strings.Cut(s, "#")
	m := arnRE.FindStringSubmatch(base)
	if m == nil {
		return arn{}, fmt.Errorf("%q is not an ssm or secretsmanager ARN (arn:aws:<service>:<region>:<account>:<resource>)", s)
	}
	a := arn{Service: m[1], Region: m[2], Base: base, Field: field}
	resource := m[4]
	switch a.Service {
	case "ssm":
		if !strings.HasPrefix(resource, "parameter/") || len(resource) == len("parameter/") {
			return arn{}, fmt.Errorf("%q: ssm ARN resource must be parameter/<name>", s)
		}
		if hasField {
			return arn{}, fmt.Errorf("%q: #field is only supported for secretsmanager", s)
		}
	case "secretsmanager":
		if !strings.HasPrefix(resource, "secret:") || len(resource) == len("secret:") {
			return arn{}, fmt.Errorf("%q: secretsmanager ARN resource must be secret:<name>", s)
		}
		if hasField && field == "" {
			return arn{}, fmt.Errorf("%q: empty #field", s)
		}
	}
	return a, nil
}

// Select returns the hosts in group (if set) unioned with the hosts whose
// name or address matches one of names, in config order. With neither
// filter it returns every host. It also returns the names that matched no
// entry, which callers treat as literal addresses.
func (c *Config) Select(group string, names []string) (hosts []Host, unmatched []string) {
	if group == "" && len(names) == 0 {
		return append([]Host(nil), c.Hosts...), nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	matched := make(map[string]bool, len(names))
	for _, h := range c.Hosts {
		byName, byAddr := want[h.Name], want[h.Host]
		if byName {
			matched[h.Name] = true
		}
		if byAddr {
			matched[h.Host] = true
		}
		if (group != "" && h.Group == group) || byName || byAddr {
			hosts = append(hosts, h)
		}
	}
	for _, n := range names {
		if !matched[n] {
			unmatched = append(unmatched, n)
		}
	}
	return hosts, unmatched
}
