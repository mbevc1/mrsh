package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
)

// maxBody bounds request bodies.
const maxBody = 1 << 20

// Secret is a user or pass field-group as the UI sees it. Kind is "unset",
// "plain", "env" or "arn". Literal usernames are shown as-is; literal
// passwords never reach the browser: Set reports that one exists, and
// sending kind "plain" with an empty value keeps the stored password.
type Secret struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Set   bool   `json:"set,omitempty"`
}

// HostView is one host in the API.
type HostView struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	Group        string `json:"group"`
	Port         int    `json:"port"`
	IdentityFile string `json:"identity_file"`
	KnownHosts   string `json:"known_hosts"`
	User         Secret `json:"user"`
	Pass         Secret `json:"pass"`
}

// DefaultsView is the defaults block in the API.
type DefaultsView struct {
	Port          int    `json:"port"`
	Timeout       int    `json:"timeout"`
	Parallel      int    `json:"parallel"`
	Output        string `json:"output"`
	Debug         bool   `json:"debug"`
	IdentityFile  string `json:"identity_file"`
	KnownHosts    string `json:"known_hosts"`
	HostKeyPolicy string `json:"host_key_policy"`
	User          Secret `json:"user"`
	Pass          Secret `json:"pass"`
}

// ConfigView is GET /api/config.
type ConfigView struct {
	Version  string       `json:"version"`
	Location string       `json:"location"`
	Defaults DefaultsView `json:"defaults"`
	Commands []string     `json:"commands"`
	Hosts    []HostView   `json:"hosts"`
	Groups   []string     `json:"groups"`
}

func toSecret(r config.SecretRef) Secret {
	switch r.Kind() {
	case config.KindPlain:
		if r.Field == "user" {
			return Secret{Kind: "plain", Value: r.Literal, Set: true}
		}
		return Secret{Kind: "plain", Set: true} // passwords stay masked
	case config.KindEnv:
		return Secret{Kind: "env", Value: r.Env}
	case config.KindARN:
		return Secret{Kind: "arn", Value: r.ARN}
	}
	return Secret{Kind: "unset"}
}

// applySecret turns a Secret back into the three config keys. For the
// masked password, kind plain with an empty value keeps the stored literal;
// a visible username that was emptied is an error instead.
func applySecret(field string, in Secret, old config.SecretRef) (literal, env, arn string, err error) {
	v := strings.TrimSpace(in.Value)
	switch in.Kind {
	case "", "unset":
		return "", "", "", nil
	case "plain":
		if in.Value != "" {
			return in.Value, "", "", nil // literals are verbatim, spaces included
		}
		if field == "pass" && old.Kind() == config.KindPlain {
			return old.Literal, "", "", nil
		}
		return "", "", "", fmt.Errorf("%s: enter a literal value", field)
	case "env":
		if v == "" {
			return "", "", "", fmt.Errorf("%s: enter an environment variable name", field)
		}
		return "", v, "", nil
	case "arn":
		if v == "" {
			return "", "", "", fmt.Errorf("%s: enter an ARN", field)
		}
		return "", "", v, nil
	}
	return "", "", "", fmt.Errorf("%s: unknown kind %q", field, in.Kind)
}

func applyCredentials(user, pass Secret, old config.Credentials) (config.Credentials, error) {
	var c config.Credentials
	var errs []error
	var err error
	if c.User, c.UserEnv, c.UserARN, err = applySecret("user", user, old.UserRef()); err != nil {
		errs = append(errs, err)
	}
	if c.Pass, c.PassEnv, c.PassARN, err = applySecret("pass", pass, old.PassRef()); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

func (v HostView) toHost(old config.Host) (config.Host, error) {
	creds, err := applyCredentials(v.User, v.Pass, old.Credentials)
	return config.Host{
		Name: strings.TrimSpace(v.Name), Host: strings.TrimSpace(v.Host), Group: strings.TrimSpace(v.Group),
		Port: v.Port, IdentityFile: strings.TrimSpace(v.IdentityFile), KnownHosts: strings.TrimSpace(v.KnownHosts),
		Credentials: creds,
	}, err
}

func viewOf(cfg *config.Config, version, location string) ConfigView {
	d := cfg.Defaults
	out := ConfigView{
		Version: version, Location: location, Commands: cfg.Commands,
		Defaults: DefaultsView{
			Port: d.Port, Timeout: d.Timeout, Parallel: d.Parallel, Output: d.Output, Debug: d.Debug,
			IdentityFile: d.IdentityFile, KnownHosts: d.KnownHosts, HostKeyPolicy: d.HostKeyPolicy,
			User: toSecret(d.UserRef()), Pass: toSecret(d.PassRef()),
		},
		Hosts: make([]HostView, 0, len(cfg.Hosts)), Groups: []string{},
	}
	if out.Commands == nil {
		out.Commands = []string{}
	}
	seen := map[string]bool{}
	for _, h := range cfg.Hosts {
		out.Hosts = append(out.Hosts, HostView{
			Name: h.Name, Host: h.Host, Group: h.Group, Port: h.Port,
			IdentityFile: h.IdentityFile, KnownHosts: h.KnownHosts,
			User: toSecret(h.UserRef()), Pass: toSecret(h.PassRef()),
		})
		if h.Group != "" && !seen[h.Group] {
			seen[h.Group] = true
			out.Groups = append(out.Groups, h.Group)
		}
	}
	return out
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// Exit: reply first, then stop the server (Serve shuts down gracefully,
	// so this reply is delivered). The guard makes it token + JSON only.
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.requestQuit()
	})
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("POST /api/validate", s.validate)
	mux.HandleFunc("POST /api/hosts", s.mutate(func(r *http.Request, d *config.Document, cfg *config.Config) error {
		var v HostView
		if err := decode(r, &v); err != nil {
			return err
		}
		h, err := v.toHost(config.Host{})
		if err != nil {
			return &config.ValidationError{Err: err}
		}
		return d.AddHost(h)
	}))
	mux.HandleFunc("PUT /api/hosts/{name}", s.mutate(func(r *http.Request, d *config.Document, cfg *config.Config) error {
		name := r.PathValue("name")
		old, ok := find(cfg, name)
		if !ok {
			return fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
		}
		var v HostView
		if err := decode(r, &v); err != nil {
			return err
		}
		h, err := v.toHost(old)
		if err != nil {
			return &config.ValidationError{Err: err}
		}
		if h.Name != name {
			if _, taken := find(cfg, h.Name); taken {
				return fmt.Errorf("%w: %s", config.ErrHostExists, h.Name)
			}
		}
		return d.ReplaceHost(name, h)
	}))
	mux.HandleFunc("DELETE /api/hosts/{name}", s.mutate(func(r *http.Request, d *config.Document, _ *config.Config) error {
		return d.RemoveHost(r.PathValue("name"))
	}))
	mux.HandleFunc("PUT /api/defaults", s.mutate(func(r *http.Request, d *config.Document, cfg *config.Config) error {
		var v DefaultsView
		if err := decode(r, &v); err != nil {
			return err
		}
		creds, err := applyCredentials(v.User, v.Pass, cfg.Defaults.Credentials)
		if err != nil {
			return &config.ValidationError{Err: err}
		}
		return d.SetDefaults(config.Defaults{
			Credentials: creds, Port: v.Port, Timeout: v.Timeout, Parallel: v.Parallel,
			Output: v.Output, Debug: v.Debug, IdentityFile: strings.TrimSpace(v.IdentityFile),
			KnownHosts: strings.TrimSpace(v.KnownHosts), HostKeyPolicy: v.HostKeyPolicy,
		})
	}))
	mux.HandleFunc("PUT /api/commands", s.mutate(func(r *http.Request, d *config.Document, _ *config.Config) error {
		var cmds []string
		if err := decode(r, &cmds); err != nil {
			return err
		}
		var clean []string
		for _, c := range cmds {
			if c = strings.TrimSpace(c); c != "" {
				clean = append(clean, c)
			}
		}
		return d.SetCommands(clean)
	}))
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	raw, version, err := s.Store.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("ETag", version)
	writeJSON(w, http.StatusOK, viewOf(cfg, version, s.Store.Location()))
}

// validate checks a host edit against the current config without saving.
// Body: {"original": "<name or empty for a new host>", "host": {...}}.
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Original string   `json:"original"`
		Host     HostView `json:"host"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, _, err := s.Store.Load(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var old config.Host
	idx := -1
	for i, h := range cfg.Hosts {
		if req.Original != "" && h.Name == req.Original {
			old, idx = h, i
		}
	}
	if req.Original != "" && idx < 0 {
		writeError(w, http.StatusNotFound, "host not found: "+req.Original)
		return
	}
	h, err := req.Host.toHost(old)
	problems := []string{}
	if err != nil {
		problems = config.Problems(err)
	} else {
		if idx >= 0 {
			cfg.Hosts[idx] = h
		} else {
			cfg.Hosts = append(cfg.Hosts, h)
		}
		if err := cfg.Validate(); err != nil {
			problems = config.Problems(err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(problems) == 0, "errors": problems})
}

// mutate wraps an edit in the version-pinned mutation cycle. The client
// sends the version it loaded in If-Match; a stale version gets 412.
func (s *Server) mutate(fn func(*http.Request, *config.Document, *config.Config) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		version := r.Header.Get("If-Match")
		if version == "" {
			writeError(w, http.StatusPreconditionRequired, "If-Match with the loaded config version is required")
			logRequest(r, http.StatusPreconditionRequired, start)
			return
		}
		next, err := config.MutateAt(r.Context(), s.Store, version, func(d *config.Document, c *config.Config) error {
			return fn(r, d, c)
		})
		status := http.StatusOK
		var ve *config.ValidationError
		switch {
		case err == nil:
			w.Header().Set("ETag", next)
			writeJSON(w, status, map[string]string{"version": next})
		case errors.Is(err, config.ErrVersionConflict):
			status = http.StatusPreconditionFailed
			writeError(w, status, "the config changed since you loaded it (another tab or the CLI); reload to see the latest")
		case errors.As(err, &ve):
			status = http.StatusUnprocessableEntity
			writeJSON(w, status, map[string]any{"error": "validation failed", "errors": ve.Problems()})
		case errors.Is(err, config.ErrHostNotFound):
			status = http.StatusNotFound
			writeError(w, status, err.Error())
		case errors.Is(err, config.ErrHostExists):
			status = http.StatusConflict
			writeError(w, status, err.Error())
		case errors.Is(err, errBadRequest):
			status = http.StatusBadRequest
			writeError(w, status, err.Error())
		default:
			status = http.StatusInternalServerError
			writeError(w, status, err.Error())
		}
		logRequest(r, status, start)
	}
}

func find(cfg *config.Config, name string) (config.Host, bool) {
	for _, h := range cfg.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return config.Host{}, false
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	return nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "config not found; create it with 'mrsh hosts init'")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
