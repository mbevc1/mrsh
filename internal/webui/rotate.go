package webui

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/mbevc1/mrsh/internal/config"
)

// RotatePlan is what a password rotation would do, for the page to confirm.
type RotatePlan struct {
	Label string   `json:"label"` // "credential routers" or "host rb02"
	Hosts []string `json:"hosts"` // devices whose password changes
	Store string   `json:"store"` // where it stays: "ssm", "literal" (the config) or "env"
	Dest  string   `json:"dest"`  // the SSM parameter or environment variable
}

// RotateResult is one device's state during or after a rotation.
type RotateResult struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
	Running bool   `json:"running,omitempty"` // a progress update; the final result follows
}

// RotateOutcome is how a rotation ended. OK means every device has the new
// password and it is stored where the old one was; otherwise Message says
// where things stand. Password is the new password whenever any device
// has it, for the page to show once.
type RotateOutcome struct {
	OK       bool           `json:"ok"`
	Message  string         `json:"message"`
	Notes    []string       `json:"notes,omitempty"`
	Password string         `json:"password,omitempty"`
	Results  []RotateResult `json:"results"`
}

// Rotator rotates the password of a credential set ("credential") or of a
// host's own password ("host") on RouterOS devices, keeping it where it is
// stored, as --rotate-pass does. Plan only describes it. Apply reports to progress
// (which may be nil) as devices move through their steps. Both return an
// error wrapping config.ErrHostNotFound or config.ErrCredentialNotFound
// for an unknown name; an Apply error means no device was changed.
type Rotator struct {
	Plan  func(ctx context.Context, kind, name string) (RotatePlan, error)
	Apply func(ctx context.Context, kind, name string, progress func(RotateResult)) (RotateOutcome, error)

	running sync.Mutex // one rotation at a time
}

type rotateRequest struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Confirm bool   `json:"confirm"`
	Stream  bool   `json:"stream"`
}

// rotateRequestFor decodes and checks a rotation request; ok is false when
// the reply has been written.
func (s *Server) rotateRequestFor(w http.ResponseWriter, r *http.Request) (*Rotator, rotateRequest, bool) {
	var req rotateRequest
	rot := s.Rotate
	if rot == nil {
		writeError(w, http.StatusNotImplemented, "rotating passwords is not available")
		return nil, req, false
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, req, false
	}
	if req.Kind != "credential" && req.Kind != "host" {
		writeError(w, http.StatusBadRequest, `"kind" must be "credential" or "host"`)
		return nil, req, false
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, `"name" is required`)
		return nil, req, false
	}
	return rot, req, true
}

func writeRotateError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrHostNotFound) || errors.Is(err, config.ErrCredentialNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusUnprocessableEntity, err.Error())
}

// rotatePlan is POST /api/rotate/plan ({"kind": "credential", "name": "routers"}).
func (s *Server) rotatePlan(w http.ResponseWriter, r *http.Request) {
	rot, req, ok := s.rotateRequestFor(w, r)
	if !ok {
		return
	}
	plan, err := rot.Plan(r.Context(), req.Kind, req.Name)
	if err != nil {
		writeRotateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// rotateApply is POST /api/rotate/apply ({"kind", "name", "confirm": true}).
// With "stream": true the reply is NDJSON: RotateResult lines as devices
// move, then {"outcome": ...} (or {"error": ...}) last. Errors found before
// any device is reached keep their status code. A rotation runs to the end
// even if the page goes away, so it never stops between devices.
func (s *Server) rotateApply(w http.ResponseWriter, r *http.Request) {
	rot, req, ok := s.rotateRequestFor(w, r)
	if !ok {
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, `rotation needs "confirm": true; device passwords change`)
		return
	}
	if !rot.running.TryLock() {
		writeError(w, http.StatusConflict, "a rotation is already running")
		return
	}
	defer rot.running.Unlock()
	var stream *ndjson
	var progress func(RotateResult)
	if req.Stream {
		stream = &ndjson{w: w}
		progress = func(res RotateResult) { stream.send(res) }
	}
	out, err := rot.Apply(context.WithoutCancel(r.Context()), req.Kind, req.Name, progress)
	if stream != nil && stream.started {
		if err != nil {
			stream.send(map[string]string{"error": err.Error()})
		} else {
			stream.send(map[string]any{"outcome": out})
		}
		return
	}
	if err != nil {
		writeRotateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcome": out})
}
