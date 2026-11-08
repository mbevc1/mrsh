package webui

import (
	"context"
	"errors"
	"net/http"

	"github.com/mbevc1/mrsh/internal/config"
)

// TestResult is one host's connection check.
type TestResult struct {
	Name       string `json:"name"`
	Host       string `json:"host"`
	OK         bool   `json:"ok"`
	Summary    string `json:"summary"` // short form for the badge, e.g. "connect: no route to host"
	Detail     string `json:"detail"`  // full message, shown on hover
	DurationMS int64  `json:"duration_ms"`
}

// Tester connects to saved config hosts by name (all of them when names is
// empty) and runs a fixed, read-only check on each. It returns an error
// wrapping config.ErrHostNotFound for an unknown name.
type Tester func(ctx context.Context, names []string) ([]TestResult, error)

// testHosts is POST /api/test with body {"hosts": ["name", ...]}.
func (s *Server) testHosts(w http.ResponseWriter, r *http.Request) {
	if s.Test == nil {
		writeError(w, http.StatusNotImplemented, "host testing is not available")
		return
	}
	var req struct {
		Hosts []string `json:"hosts"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, err := s.Test(r.Context(), req.Hosts)
	switch {
	case errors.Is(err, config.ErrHostNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}
