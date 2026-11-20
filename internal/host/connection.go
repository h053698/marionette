package host

import (
	"context"
	"net/http"
	"strings"
	"time"

	"marionette/internal/identity"
)

// Operations are scoped to one physical device. Disconnect is a persistent
// Marionette control setting, so the USB device stays visible to administrators.
func (s *Server) adminDeviceConnection(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator role required")
		return
	}
	var request struct {
		Action string `json:"action"`
	}
	if readJSON(w, r, &request) != nil || request.Action != "debug" && request.Action != "disconnect" && request.Action != "reconnect" {
		fail(w, 400, "invalid device connection action")
		return
	}
	serial := r.PathValue("serial")
	s.mu.Lock()
	s.expireLocked()
	d := s.devices[serial]
	switch {
	case s.closed:
		s.mu.Unlock()
		fail(w, 503, "agent is closing")
		return
	case d == nil || !s.allows(serial):
		s.mu.Unlock()
		fail(w, 404, "unknown device")
		return
	case s.cfg.Machine == nil || d.device.Kind != "physical":
		s.mu.Unlock()
		fail(w, 409, "connection management requires a physical device on a supported agent")
		return
	case d.lease != nil || d.replacing || d.connectionChanging || d.leaseCleanup != nil || s.wipeStateLocked(d) != "":
		s.mu.Unlock()
		fail(w, 409, "device is in use or cleanup is in progress")
		return
	}
	if request.Action == "disconnect" || request.Action == "reconnect" {
		disconnect := request.Action == "disconnect"
		if err := s.cfg.Machine.SetDisconnected(serial, disconnect); err != nil {
			s.mu.Unlock()
			fail(w, 500, "could not save device connection setting")
			return
		}
		d.debugRequestedUntil = time.Time{}
		state := d.device.State
		if disconnect {
			s.invalidateLocked(d)
			state = "disconnected"
		} else if state == "device" && d.online == nil {
			d.online = make(chan struct{})
		}
		s.reconcileLocked(d)
		s.mu.Unlock()
		writeJSON(w, 200, map[string]string{"state": state})
		return
	}
	if s.disconnected(serial) {
		s.mu.Unlock()
		fail(w, 409, "reconnect the device in Marionette first")
		return
	}
	state, target := d.device.State, d.device.target()
	if state == "device" {
		s.mu.Unlock()
		writeJSON(w, 200, map[string]string{"state": state})
		return
	}
	if !d.adbTransportPresent {
		s.mu.Unlock()
		writeJSON(w, 200, map[string]string{"state": "debug_unavailable"})
		return
	}
	d.connectionChanging = true
	s.invalidateLocked(d)
	s.reconcileLocked(d)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	output, err := s.cfg.ADB.Run(ctx, target.args("reconnect")...)
	cancel()
	reconnecting := err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), "reconnecting ")
	s.mu.Lock()
	d.connectionChanging = false
	if reconnecting && !s.closed && d.device.State != "device" {
		d.debugRequestedUntil = time.Now().Add(45 * time.Second)
	}
	if !s.closed && s.deviceOnlineLocked(d) && d.online == nil {
		d.online = make(chan struct{})
	}
	s.reconcileLocked(d)
	s.mu.Unlock()
	if !reconnecting {
		fail(w, 409, "ADB could not reconnect this device; check the USB connection and try again")
		return
	}
	writeJSON(w, 202, map[string]string{"state": "debug_requested"})
}
