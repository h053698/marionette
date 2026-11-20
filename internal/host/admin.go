package host

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"marionette/internal/identity"
)

const (
	leaseMaintenance = "maintenance"
	leaseTemporary   = "temporary"
	temporaryControl = 2 * time.Minute
)

// A public lease ID identifies the occupancy the administrator reviewed. It is
// never a control credential; a changed lease must be reviewed again.
func (s *Server) adminLease(w http.ResponseWriter, r *http.Request) {
	s.changeAdminLease(w, r, false)
}

func (s *Server) adminControlLease(w http.ResponseWriter, r *http.Request) {
	s.changeAdminLease(w, r, true)
}

func (s *Server) changeAdminLease(w http.ResponseWriter, r *http.Request, controlMode bool) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator role required")
		return
	}
	var body struct {
		LeaseID *string `json:"leaseId"`
		TTL     int     `json:"ttlSeconds"`
		Mode    string  `json:"mode"`
	}
	if err := readJSON(w, r, &body); err != nil || body.LeaseID == nil || (!controlMode && *body.LeaseID == "") {
		fail(w, 400, "current leaseId is required")
		return
	}
	if (controlMode && body.Mode != leaseMaintenance && body.Mode != leaseTemporary) || (!controlMode && body.Mode != "") {
		fail(w, 400, "invalid administrator control mode")
		return
	}
	if body.Mode == leaseTemporary && *body.LeaseID != "" {
		fail(w, 409, "temporary control requires an unoccupied device")
		return
	}
	duration, err := s.cfg.Leases.Duration(body.TTL)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if body.Mode == leaseTemporary {
		duration = min(temporaryControl, s.cfg.Leases.MaxTTL)
	}
	user, _ := identity.FromContext(r.Context())
	owner := strings.TrimSpace(user.Name)
	if owner == "" {
		owner = strings.TrimSpace(user.Email)
	}
	if owner == "" {
		owner = user.Subject
	}
	if r.Method == http.MethodPost && len(owner) > 200 {
		fail(w, 400, "owner must contain 1–200 bytes")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if r.Context().Err() != nil {
		fail(w, 408, "administrator lease request canceled")
		return
	}
	d := s.devices[r.PathValue("serial")]
	if s.closed {
		fail(w, 503, "server shutting down")
		return
	}
	if d == nil {
		fail(w, 404, "unknown device")
		return
	}
	currentID := ""
	if d.lease != nil {
		currentID = d.lease.ID
	}
	if currentID != *body.LeaseID {
		fail(w, 409, "device occupancy changed; refresh and try again")
		return
	}
	if r.Method == http.MethodPost && (!s.deviceOnlineLocked(d) || !s.allows(d.device.Serial) || d.replacing) {
		fail(w, 409, "device is not authorized and online")
		return
	}
	previous := d.lease
	if previous != nil && previous.Mode != "" && r.Method == http.MethodPost {
		fail(w, 409, "end the existing administrator session before starting another mode")
		return
	}
	target := d.device.target()
	if r.Method == http.MethodPost {
		for _, other := range s.devices {
			if other != d && other.lease != nil && other.lease.subject == user.Subject {
				fail(w, 409, "return your current device before acquiring another")
				return
			}
		}
	}
	operation := "disconnect"
	if r.Method == http.MethodPost {
		operation = "takeover"
		if controlMode {
			operation = body.Mode
		}
		d.replacing = true
		defer func() { d.replacing = false }()
	}
	s.waitCleanupLocked(r.Context(), s.revokeLeaseLocked(d))
	if previous != nil {
		slog.Info("administrator ended device lease", "serial", d.device.Serial, "administrator", user.Subject,
			"previous_subject", previous.subject, "operation", operation)
	}
	if r.Method == http.MethodPost {
		if r.Context().Err() != nil || s.devices[d.device.Serial] != d || d.device.target() != target || !s.canGrantLocked(d) {
			fail(w, 409, "device is unavailable or lease cleanup is incomplete")
			return
		}
		for _, other := range s.devices {
			if other.lease != nil && other.lease.subject == user.Subject {
				fail(w, 409, "return your current device before acquiring another")
				return
			}
		}
		granted := d.grant(owner, user.Subject, duration)
		granted.Mode = body.Mode
		if controlMode {
			slog.Info("administrator started device control", "serial", d.device.Serial, "administrator", user.Subject, "mode", body.Mode)
		}
		writeJSON(w, 201, granted)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// adminLeases lists who holds each active lease so the backend can join users to
// devices. It carries neither lease IDs nor tokens, and is reachable only through
// the identity-injecting tunnel by administrators.
func (s *Server) adminLeases(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator role required")
		return
	}
	type holder struct {
		Subject   string    `json:"subject"`
		Serial    string    `json:"serial"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	leases := make([]holder, 0)
	s.mu.Lock()
	s.expireLocked()
	for _, d := range s.devices {
		if d.lease == nil || d.lease.subject == "" || !s.allows(d.device.Serial) {
			continue
		}
		leases = append(leases, holder{Subject: d.lease.subject, Serial: d.device.Serial, ExpiresAt: d.lease.ExpiresAt})
	}
	s.mu.Unlock()
	sort.Slice(leases, func(i, j int) bool { return leases[i].Serial < leases[j].Serial })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"leases": leases})
}
