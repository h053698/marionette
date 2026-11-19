package host

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"marionette/internal/identity"
)

// Handoff codes transfer one existing lease to a CLI belonging to the same OIDC
// subject. They expire in 90 seconds and are invalidated on the first use.
func (s *Server) handoff(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		return
	}
	code := randomID()
	d.lease.handoffHash = sha256.Sum256([]byte(code))
	d.lease.handoffUntil = time.Now().Add(90 * time.Second)
	writeJSON(w, 200, map[string]any{"code": code, "expiresAt": d.lease.handoffUntil})
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil || len(input.Code) != 48 {
		fail(w, 400, "invalid connection code")
		return
	}
	user, ok := identity.FromContext(r.Context())
	if !ok {
		fail(w, 401, "sign-in required")
		return
	}
	hash := sha256.Sum256([]byte(input.Code))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	d := s.devices[r.PathValue("serial")]
	if d == nil || d.lease == nil || d.device.State != "device" || d.lease.subject != user.Subject || !time.Now().Before(d.lease.handoffUntil) || subtle.ConstantTimeCompare(hash[:], d.lease.handoffHash[:]) != 1 {
		fail(w, 403, "connection code expired or belongs to another account")
		return
	}
	previous := d.lease
	d.replacing = true
	defer func() { d.replacing = false }()
	s.waitCleanupLocked(r.Context(), s.revokeLeaseLocked(d))
	if r.Context().Err() != nil || d.device.target() != previous.target || !s.canGrantLocked(d) {
		fail(w, 409, "device is unavailable or lease cleanup is incomplete")
		return
	}
	for _, other := range s.devices {
		if other.lease != nil && other.lease.subject == user.Subject {
			fail(w, 409, "return your current device before acquiring another")
			return
		}
	}
	writeJSON(w, 200, d.grant(previous.Owner, user.Subject, s.cfg.Leases.DefaultTTL))
}
