package webui

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/mbevc1/mrsh/internal/config"
)

// UpgradeResult is one device's upgrade step, done or (dry run) planned.
type UpgradeResult struct {
	Name       string `json:"name"`
	Host       string `json:"host"`
	OK         bool   `json:"ok"`
	Action     string `json:"action"` // install, firmware or none
	Summary    string `json:"summary"`
	Detail     string `json:"detail"`
	DurationMS int64  `json:"duration_ms"`
}

// Upgrader runs the next upgrade step on saved hosts, as `mt upgrade` does.
// Dry only reports what Apply would do. Both return an error wrapping
// config.ErrHostNotFound for an unknown name.
type Upgrader struct {
	Dry   func(ctx context.Context, names []string) ([]UpgradeResult, error)
	Apply func(ctx context.Context, names []string) ([]UpgradeResult, error)

	running sync.Mutex // one apply at a time
}

// upgradeHosts is POST /api/upgrade/dry ({"hosts": [...]}) and
// POST /api/upgrade/apply ({"hosts": [...], "confirm": true}).
func (s *Server) upgradeHosts(apply bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.Upgrade
		if u == nil {
			writeError(w, http.StatusNotImplemented, "upgrading is not available")
			return
		}
		var req struct {
			Hosts   []string `json:"hosts"`
			Confirm bool     `json:"confirm"`
		}
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// An empty list means "all hosts" elsewhere; never for an upgrade.
		if len(req.Hosts) == 0 {
			writeError(w, http.StatusBadRequest, "no hosts selected")
			return
		}
		run := u.Dry
		if apply {
			if !req.Confirm {
				writeError(w, http.StatusBadRequest, "upgrade needs \"confirm\": true; devices reboot")
				return
			}
			if !u.running.TryLock() {
				writeError(w, http.StatusConflict, "an upgrade is already running")
				return
			}
			defer u.running.Unlock()
			run = u.Apply
		}
		results, err := run(r.Context(), req.Hosts)
		switch {
		case errors.Is(err, config.ErrHostNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeJSON(w, http.StatusOK, map[string]any{"results": results})
		}
	}
}
