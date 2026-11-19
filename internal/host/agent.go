package host

import (
	"marionette/internal/identity"
	"marionette/internal/machine"
	"net/http"
)

func (s *Server) agentStatus(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator required")
		return
	}
	if s.cfg.Machine == nil {
		writeJSON(w, 200, map[string]any{"mode": "physical", "emulatorAvailable": false, "configurable": false})
		return
	}
	status := struct {
		machine.Snapshot
		ControllersReady int             `json:"controllersReady"`
		Viewers          int             `json:"viewers"`
		Metrics          machine.Metrics `json:"metrics"`
	}{Snapshot: s.cfg.Machine.Snapshot(), Metrics: s.cfg.Machine.Metrics()}
	s.mu.Lock()
	for _, device := range s.devices {
		if device.runner != nil && device.runner.alive() {
			status.ControllersReady++
			status.Viewers += device.runner.viewerCount()
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, status)
}

func (s *Server) agentPolicy(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator required")
		return
	}
	if s.cfg.Machine == nil {
		fail(w, 409, "this agent does not support Android operation policies")
		return
	}
	var policy machine.Policy
	if err := readJSON(w, r, &policy); err != nil {
		fail(w, 400, "invalid agent policy JSON")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		fail(w, 503, "agent is closing")
		return
	}
	s.expireLocked()
	for _, device := range s.devices {
		if device.lease != nil {
			fail(w, 409, "return all devices on this agent before changing its operation policy")
			return
		}
	}
	warmupEnabled := policy.PreWarm && !s.cfg.Machine.Snapshot().PreWarm
	if err := s.cfg.Machine.Update(policy); err != nil {
		fail(w, 400, err.Error())
		return
	}
	for _, device := range s.devices {
		// Existing preview/interactive processes may not have attached
		// UiAutomation yet. Recreate them once when pre-warm is enabled.
		if warmupEnabled {
			device.reset = true
		}
		s.reconcileLocked(device)
	}
	writeJSON(w, 200, s.cfg.Machine.Snapshot())
}
