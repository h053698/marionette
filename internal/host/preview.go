package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"marionette/internal/identity"
)

// All demand and runner fields are guarded by Server.mu. Slow device work is
// serialized by one worker per slot, never under the global lock. Each worker rechecks demand
// after external work, so release/discovery/start races converge on current state.
func (s *Server) desiredLocked(d *slot) string {
	if s.closed || s.devices[d.device.Serial] != d || d.device.State != "device" || !s.allows(d.device.Serial) || s.quarantinedLocked(d) || s.wipeStateLocked(d) != "" {
		return ""
	}
	if s.cfg.Machine != nil {
		if s.cfg.Machine.PreWarm(d.device.Serial) {
			return "interactive"
		}
	}
	if d.interactive && d.lease != nil && time.Now().Before(d.lease.ExpiresAt) {
		return "interactive"
	}
	if d.previews > 0 {
		return "preview"
	}
	return ""
}

type runtimeWork struct {
	done   chan struct{}
	err    error // Published before done closes; immutable afterwards.
	cancel context.CancelFunc
}

func (s *Server) reconcileLocked(d *slot) *runtimeWork {
	d.revision++
	result := &runtimeWork{done: make(chan struct{})}
	if s.closed {
		result.err = errors.New("server shutting down")
		close(result.done)
		return result
	}
	if d.leaseCleanup != nil {
		result.err = errors.New("lease cleanup in progress")
		close(result.done)
		return result
	}
	if d.work != nil {
		return d.work
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	result.cancel = cancel
	d.work = result
	s.cleanup.Add(1)
	go func() {
		defer s.cleanup.Done()
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				s.mu.Lock()
				result.err = errors.New("device runtime worker failed")
				d.work = nil
				close(result.done)
				s.mu.Unlock()
				// Log the panic type and stack without logging application data.
				func() { defer logPanic("device runtime"); panic(p) }()
			}
		}()
		for {
			s.mu.Lock()
			revision := d.revision
			s.mu.Unlock()
			err := s.reconcile(ctx, d)
			s.mu.Lock()
			if d.revision != revision && !s.closed && ctx.Err() == nil {
				s.mu.Unlock()
				continue
			}
			result.err = err
			d.work = nil
			close(result.done)
			s.mu.Unlock()
			return
		}
	}()
	return result
}

func (s *Server) reconcile(ctx context.Context, d *slot) error {
	for {
		s.mu.Lock()
		if d.leaseCleanup != nil {
			s.mu.Unlock()
			return nil
		}
		desired := s.desiredLocked(d)
		runner, profile := d.runner, d.profile
		reset := d.reset
		if runner != nil && (reset || !runner.alive() || desired == "") {
			d.runner = nil
			d.profile = ""
			s.mu.Unlock()
			runner.Close()
			continue
		}
		d.reset = false
		if desired == "" || (runner != nil && profile == desired) {
			s.mu.Unlock()
			return nil
		}
		target := d.device.target()
		online := d.online
		s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if runner == nil {
			startCtx, cancel := context.WithCancel(ctx)
			go func() {
				select {
				case <-online:
					cancel()
				case <-startCtx.Done():
				}
			}()
			created, err := startRunnerProfile(startCtx, s.cfg.ADB, target, s.cfg.DeviceJar, desired)
			cancel()
			if err != nil {
				return err
			}
			s.mu.Lock()
			// Close() may already be waiting for this worker. Never publish a
			// runner after shutdown or device disappearance.
			if s.desiredLocked(d) == "" || d.online != online {
				s.mu.Unlock()
				created.Close()
				continue
			}
			d.runner, d.profile = created, desired
			s.mu.Unlock()
			continue
		}
		name, _ := json.Marshal(desired)
		raw, err := runner.Action(ctx, map[string]json.RawMessage{"action": json.RawMessage(`"videoProfile"`), "profile": name})
		var response struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &response)
			if err == nil && !response.OK {
				err = fmt.Errorf("video profile rejected: %s", response.Error)
			}
		}
		if err == nil && desired == "interactive" {
			err = runner.warmup(ctx)
		}
		if err != nil {
			runner.Close()
			s.mu.Lock()
			if d.runner == runner {
				d.runner = nil
				d.profile = ""
			}
			s.mu.Unlock()
			return err
		}
		s.mu.Lock()
		if d.runner == runner {
			d.profile = desired
		}
		s.mu.Unlock()
	}
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	// This route fails closed even when used without the production auth wrapper.
	if !identity.IsAdmin(r.Context()) {
		fail(w, http.StatusForbidden, "administrator role required")
		return
	}
	s.mu.Lock()
	s.expireLocked()
	d := s.devices[r.PathValue("serial")]
	if s.closed {
		s.mu.Unlock()
		fail(w, 503, "server shutting down")
		return
	}
	if d == nil {
		s.mu.Unlock()
		fail(w, 404, "unknown device")
		return
	}
	if d.device.State != "device" || !s.allows(d.device.Serial) {
		s.mu.Unlock()
		fail(w, 409, "device is not authorized and online")
		return
	}
	if s.wipeStateLocked(d) != "" {
		s.mu.Unlock()
		fail(w, 409, "device cleanup is incomplete")
		return
	}
	if !s.reserveVideoLocked(w, d, nil) {
		s.mu.Unlock()
		return
	}
	d.previews++
	online := d.online
	ready := s.reconcileLocked(d)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		d.previews--
		s.releaseVideoLocked(d, nil)
		s.reconcileLocked(d)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	select {
	case <-ready.done:
		if ready.err != nil {
			fail(w, 502, fmt.Sprintf("preview start failed: %v", ready.err))
			return
		}
	case <-ctx.Done():
		fail(w, 504, "preview startup canceled or timed out")
		return
	}
	s.mu.Lock()
	runner := d.runner
	valid := s.desiredLocked(d) != "" && d.online == online
	s.mu.Unlock()
	if !valid || runner == nil || !runner.alive() {
		fail(w, 409, "device unavailable")
		return
	}
	// No lease is acquired or renewed. Incoming WebSocket messages are ignored;
	// device commands are available only on the subject-bound lease endpoints.
	s.stream(w, r, runner, online)
}

func (s *Server) previewKeyframe(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator role required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		fail(w, 503, "server shutting down")
		return
	}
	d := s.devices[r.PathValue("serial")]
	if d == nil {
		fail(w, 404, "unknown device")
		return
	}
	if d.device.State != "device" || s.wipeStateLocked(d) != "" || d.runner == nil || !d.runner.alive() {
		fail(w, 409, "no active device runner")
		return
	}
	d.runner.requestKeyframe()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
