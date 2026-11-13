package webui

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mbevc1/mrsh/internal/config"
)

// HostChanges is a bulk edit: each field that is present is set on every
// selected host, and absent fields are left as they are. User and Pass take
// kinds "unset" (back to inherited), "plain", "env" or "arn"; SSM storing is
// one host at a time, and a literal must be given in full.
type HostChanges struct {
	Group        *string `json:"group,omitempty"`
	Credential   *string `json:"credential,omitempty"`
	Port         *int    `json:"port,omitempty"`
	IdentityFile *string `json:"identity_file,omitempty"`
	KnownHosts   *string `json:"known_hosts,omitempty"`
	User         *Secret `json:"user,omitempty"`
	Pass         *Secret `json:"pass,omitempty"`
}

// apply sets the present fields on h.
func (c HostChanges) apply(h *config.Host) error {
	trim := func(p *string) string { return strings.TrimSpace(*p) }
	if c.Group != nil {
		h.Group = trim(c.Group)
	}
	if c.Credential != nil {
		h.Credential = trim(c.Credential)
	}
	if c.Port != nil {
		h.Port = *c.Port
	}
	if c.IdentityFile != nil {
		h.IdentityFile = trim(c.IdentityFile)
	}
	if c.KnownHosts != nil {
		h.KnownHosts = trim(c.KnownHosts)
	}
	var errs []error
	for _, x := range []struct {
		field                string
		in                   *Secret
		literal, env, arnVal *string
	}{
		{"user", c.User, &h.User, &h.UserEnv, &h.UserARN},
		{"pass", c.Pass, &h.Pass, &h.PassEnv, &h.PassARN},
	} {
		if x.in == nil {
			continue
		}
		if x.in.Kind == "ssm" {
			errs = append(errs, fmt.Errorf("%s: storing in SSM is one host at a time; for several hosts, store it in a credential set and choose that set", x.field))
			continue
		}
		// No old value: an empty literal is an error, never "keep".
		l, e, a, err := applySecret(x.field, *x.in, config.SecretRef{})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*x.literal, *x.env, *x.arnVal = l, e, a
	}
	return errors.Join(errs...)
}

func (c HostChanges) empty() bool {
	return c.Group == nil && c.Credential == nil && c.Port == nil && c.IdentityFile == nil &&
		c.KnownHosts == nil && c.User == nil && c.Pass == nil
}

// bulkHosts checks a bulk request's host list against the config.
func bulkHosts(cfg *config.Config, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("%w: no hosts selected", errBadRequest)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if _, ok := find(cfg, n); !ok {
			return fmt.Errorf("%w: %s", config.ErrHostNotFound, n)
		}
		if seen[n] {
			return fmt.Errorf("%w: %s is listed twice", errBadRequest, n)
		}
		seen[n] = true
	}
	return nil
}

// bulkEditHosts is POST /api/hosts/bulk-edit
// ({"hosts": [...], "changes": {...}}): one config write for all of them.
func (s *Server) bulkEditHosts(r *http.Request, d *config.Document, cfg *config.Config) error {
	var req struct {
		Hosts   []string    `json:"hosts"`
		Changes HostChanges `json:"changes"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if err := bulkHosts(cfg, req.Hosts); err != nil {
		return err
	}
	if req.Changes.empty() {
		return fmt.Errorf("%w: nothing to change", errBadRequest)
	}
	for _, name := range req.Hosts {
		h, _ := find(cfg, name)
		if err := req.Changes.apply(&h); err != nil {
			return &config.ValidationError{Err: err}
		}
		if err := d.ReplaceHost(name, h); err != nil {
			return err
		}
	}
	return nil
}

// bulkDeleteHosts is POST /api/hosts/bulk-delete
// ({"hosts": [...], "delete_secret": false}). With delete_secret, their own
// SSM parameters that nothing left uses are deleted once the removal is saved.
func (s *Server) bulkDeleteHosts(r *http.Request, d *config.Document, cfg *config.Config) error {
	var req struct {
		Hosts        []string `json:"hosts"`
		DeleteSecret bool     `json:"delete_secret"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if err := bulkHosts(cfg, req.Hosts); err != nil {
		return err
	}
	if req.DeleteSecret {
		del, kept := cfg.HostSecretDeletes(req.Hosts)
		if err := s.deleteSecretsAfterSave(r, cfg, del, kept); err != nil {
			return err
		}
	}
	for _, name := range req.Hosts {
		if err := d.RemoveHost(name); err != nil {
			return err
		}
	}
	return nil
}
