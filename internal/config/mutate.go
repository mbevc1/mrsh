package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrHostExists and ErrHostNotFound report name clashes in mutations;
// ErrCredentialNotFound reports a missing credential set.
var (
	ErrHostExists         = errors.New("host already exists")
	ErrHostNotFound       = errors.New("host not found")
	ErrCredentialNotFound = errors.New("credential set not found")
)

// Document is a config held as a YAML node tree, so mutations keep the
// file's comments and layout outside the entries they change.
type Document struct {
	root yaml.Node
}

// ParseDocument parses raw as both a Document and a Config (with the same
// strict checks as Parse).
func ParseDocument(raw []byte) (*Document, *Config, error) {
	cfg, err := Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	d := &Document{}
	if err := yaml.Unmarshal(raw, &d.root); err != nil {
		return nil, nil, err
	}
	if d.root.Kind == 0 { // empty file
		d.root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if d.root.Kind != yaml.DocumentNode || len(d.root.Content) != 1 || d.root.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("config must be a YAML mapping")
	}
	return d, cfg, nil
}

// hosts returns the hosts sequence node, creating it when absent.
func (d *Document) hosts() (*yaml.Node, error) {
	m := d.root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "hosts" {
			seq := m.Content[i+1]
			switch {
			case seq.Kind == yaml.SequenceNode:
				return seq, nil
			case seq.Kind == yaml.ScalarNode && seq.Tag == "!!null":
				*seq = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				return seq, nil
			}
			return nil, errors.New("config: hosts must be a list")
		}
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hosts"}, seq)
	return seq, nil
}

// index returns the position of the host named name, or -1.
func index(seq *yaml.Node, name string) int {
	for i, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == "name" && item.Content[j+1].Value == name {
				return i
			}
		}
	}
	return -1
}

func encodeHost(h Host) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(h); err != nil {
		return nil, err
	}
	return &n, nil
}

// AddHost appends h. It fails with ErrHostExists when the name is taken.
func (d *Document) AddHost(h Host) error {
	seq, err := d.hosts()
	if err != nil {
		return err
	}
	if index(seq, h.Name) >= 0 {
		return fmt.Errorf("%w: %s (use hosts update)", ErrHostExists, h.Name)
	}
	n, err := encodeHost(h)
	if err != nil {
		return err
	}
	seq.Style = 0 // a starter "hosts: []" becomes a block list
	seq.Content = append(seq.Content, n)
	return nil
}

// ReplaceHost swaps the entry named name for h, keeping its position and
// the comments attached to it.
func (d *Document) ReplaceHost(name string, h Host) error {
	seq, err := d.hosts()
	if err != nil {
		return err
	}
	i := index(seq, name)
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrHostNotFound, name)
	}
	n, err := encodeHost(h)
	if err != nil {
		return err
	}
	old := seq.Content[i]
	n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
	seq.Content[i] = n
	return nil
}

// RemoveHost deletes the entry named name.
func (d *Document) RemoveHost(name string) error {
	seq, err := d.hosts()
	if err != nil {
		return err
	}
	i := index(seq, name)
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrHostNotFound, name)
	}
	seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
	return nil
}

// credentials returns the top-level credentials mapping, or nil when absent.
func (d *Document) credentials() (*yaml.Node, error) {
	m := d.root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "credentials" {
			if c := m.Content[i+1]; c.Kind == yaml.MappingNode {
				return c, nil
			}
			return nil, errors.New("config: credentials must be a mapping of name to set")
		}
	}
	return nil, nil
}

// SetCredential adds or replaces the named credential set, keeping the
// comments and position of an existing one. A new credentials section goes
// after defaults.
func (d *Document) SetCredential(name string, set CredentialSet) error {
	var n yaml.Node
	if err := n.Encode(set); err != nil {
		return err
	}
	creds, err := d.credentials()
	if err != nil {
		return err
	}
	if creds == nil {
		creds = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if err := d.setTop("credentials", creds, 1); err != nil {
			return err
		}
	}
	for i := 0; i+1 < len(creds.Content); i += 2 {
		if creds.Content[i].Value == name {
			old := creds.Content[i+1]
			n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
			creds.Content[i+1] = &n
			return nil
		}
	}
	creds.Style = 0
	creds.Content = append(creds.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, &n)
	return nil
}

// SetDefaultCredential sets (or with "" removes) defaults.credential,
// editing the defaults mapping in place so its other keys and comments stay.
func (d *Document) SetDefaultCredential(name string) error {
	m := d.root.Content[0]
	var defs *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "defaults" {
			defs = m.Content[i+1]
		}
	}
	if defs == nil || (defs.Kind == yaml.ScalarNode && defs.Tag == "!!null") {
		if name == "" {
			return nil
		}
		if defs == nil {
			defs = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if err := d.setTop("defaults", defs, 0); err != nil {
				return err
			}
		} else {
			*defs = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
	}
	if defs.Kind != yaml.MappingNode {
		return errors.New("config: defaults must be a mapping")
	}
	for i := 0; i+1 < len(defs.Content); i += 2 {
		if defs.Content[i].Value == "credential" {
			if name == "" {
				defs.Content = append(defs.Content[:i], defs.Content[i+2:]...)
			} else {
				defs.Content[i+1].Value, defs.Content[i+1].Tag, defs.Content[i+1].Style = name, "!!str", 0
			}
			return nil
		}
	}
	if name != "" {
		defs.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "credential"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
		}, defs.Content...)
	}
	return nil
}

// RemoveCredential deletes the named set, and the credentials section once
// it is empty. References to it are left for Validate to report.
func (d *Document) RemoveCredential(name string) error {
	creds, err := d.credentials()
	if err != nil {
		return err
	}
	if creds != nil {
		for i := 0; i+1 < len(creds.Content); i += 2 {
			if creds.Content[i].Value == name {
				creds.Content = append(creds.Content[:i], creds.Content[i+2:]...)
				if len(creds.Content) == 0 {
					d.deleteTop("credentials")
				}
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s", ErrCredentialNotFound, name)
}

// Bytes encodes the document.
func (d *Document) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&d.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mutateAttempts bounds reload-and-reapply after concurrent writes.
const mutateAttempts = 3

// Mutate runs the read-modify-write cycle: Load, parse, apply fn, validate
// the result, then Save conditioned on the loaded version. When another
// writer got in first it reloads and reapplies fn, so fn must depend only on
// its arguments.
func Mutate(ctx context.Context, store ConfigStore, fn func(*Document, *Config) error) error {
	for attempt := 1; ; attempt++ {
		raw, version, err := store.Load(ctx)
		if err != nil {
			return fmt.Errorf("load config %s: %w", store.Location(), err)
		}
		doc, cfg, err := ParseDocument(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", store.Location(), err)
		}
		if err := fn(doc, cfg); err != nil {
			return err
		}
		out, err := doc.Bytes()
		if err != nil {
			return err
		}
		updated, err := Parse(out)
		if err != nil {
			return fmt.Errorf("re-parse mutated config: %w", err)
		}
		if err := updated.Validate(); err != nil {
			return fmt.Errorf("change rejected:\n%w", err)
		}
		err = store.Save(ctx, out, version)
		if errors.Is(err, ErrVersionConflict) && attempt < mutateAttempts {
			slog.Debug("config changed during update; reloading", "path", store.Location(), "attempt", attempt)
			continue
		}
		return err
	}
}

// MutateAt is Mutate pinned to the version the caller last saw (such as a
// UI tab): if the stored config moved on, it returns ErrVersionConflict
// instead of reapplying, so the caller can show the newer state first.
// It returns the new version.
func MutateAt(ctx context.Context, store ConfigStore, version string, fn func(*Document, *Config) error) (string, error) {
	raw, current, err := store.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("load config %s: %w", store.Location(), err)
	}
	if current != version {
		return "", ErrVersionConflict
	}
	doc, cfg, err := ParseDocument(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", store.Location(), err)
	}
	if err := fn(doc, cfg); err != nil {
		return "", err
	}
	out, err := doc.Bytes()
	if err != nil {
		return "", err
	}
	updated, err := Parse(out)
	if err != nil {
		return "", fmt.Errorf("re-parse mutated config: %w", err)
	}
	if err := updated.Validate(); err != nil {
		return "", &ValidationError{Err: err}
	}
	if err := store.Save(ctx, out, version); err != nil {
		return "", err
	}
	_, next, err := store.Load(ctx)
	return next, err
}

// ValidationError wraps a rejected change's validation problems.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return "change rejected:\n" + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Problems lists the individual validation messages.
func (e *ValidationError) Problems() []string { return Problems(e.Err) }

// Problems splits a joined validation error into its messages.
func Problems(err error) []string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// SetDefaults replaces the defaults block.
func (d *Document) SetDefaults(def Defaults) error {
	var n yaml.Node
	if err := n.Encode(def); err != nil {
		return err
	}
	return d.setTop("defaults", &n, 0)
}

// SetCommands replaces the commands list (removing it when empty).
func (d *Document) SetCommands(cmds []string) error {
	if len(cmds) == 0 {
		d.deleteTop("commands")
		return nil
	}
	var n yaml.Node
	if err := n.Encode(cmds); err != nil {
		return err
	}
	return d.setTop("commands", &n, 1)
}

// setTop replaces a top-level value, keeping its comments, or inserts it
// at position pos.
func (d *Document) setTop(key string, n *yaml.Node, pos int) error {
	m := d.root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			n.HeadComment, n.LineComment, n.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[i+1] = n
			return nil
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	at := min(pos*2, len(m.Content))
	m.Content = append(m.Content[:at], append([]*yaml.Node{k, n}, m.Content[at:]...)...)
	return nil
}

func (d *Document) deleteTop(key string) {
	m := d.root.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
