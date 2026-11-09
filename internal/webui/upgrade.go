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
	Name       string   `json:"name"`
	Host       string   `json:"host"`
	OK         bool     `json:"ok"`
	Action     string   `json:"action"`          // install, firmware or none: the next step
	Steps      []string `json:"steps,omitempty"` // steps taken when waiting through reboots
	Summary    string   `json:"summary"`
	Detail     string   `json:"detail"`
	DurationMS int64    `json:"duration_ms"`
	Running    bool     `json:"running,omitempty"` // a progress update; the final result follows
}

// Progress receives updates while devices are upgraded: Running entries as a
// device moves through its steps, then each device's final result as soon
// as it is done. It may be called from several goroutines.
type Progress func(UpgradeResult)

// Upgrader runs the next upgrade step on saved hosts, as `mt upgrade` does,
// or with wait every step, as `mt upgrade --wait` does. Dry only reports
// what Apply would do next. Both report to progress (which may be nil) as
// they go, and return an error wrapping config.ErrHostNotFound for an
// unknown name.
type Upgrader struct {
	Dry   func(ctx context.Context, names []string, progress Progress) ([]UpgradeResult, error)
	Apply func(ctx context.Context, names []string, wait bool, progress Progress) ([]UpgradeResult, error)

	running sync.Mutex // one apply at a time
}

// upgradeHosts is POST /api/upgrade/dry ({"hosts": [...]}) and
// POST /api/upgrade/apply ({"hosts": [...], "confirm": true, "wait": false}).
// With "stream": true the reply is NDJSON: one UpgradeResult per line as
// devices progress and finish, then {"results": [...]} (or {"error": ...})
// last. Errors found before any device is reached keep their status code.
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
			Wait    bool     `json:"wait"`
			Stream  bool     `json:"stream"`
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
		run := func(ctx context.Context, names []string, p Progress) ([]UpgradeResult, error) {
			return u.Dry(ctx, names, p)
		}
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
			run = func(ctx context.Context, names []string, p Progress) ([]UpgradeResult, error) {
				return u.Apply(ctx, names, req.Wait, p)
			}
		}
		var stream *ndjson
		var progress Progress
		if req.Stream {
			stream = &ndjson{w: w}
			progress = func(res UpgradeResult) { stream.send(res) }
		}
		results, err := run(r.Context(), req.Hosts, progress)
		if stream != nil && stream.started {
			if err != nil {
				stream.send(map[string]string{"error": err.Error()})
			} else {
				stream.send(map[string]any{"results": results})
			}
			return
		}
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
