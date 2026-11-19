package backend

import (
	"encoding/json"
	"net/http"
	"time"

	"marionette/internal/auth"
	"marionette/internal/identity"
)

// EnableMembers supplies the known-user registry behind GET /api/admin/users.
func (s *Server) EnableMembers(list func() []auth.Member) {
	s.mu.Lock()
	s.members = list
	s.mu.Unlock()
}

type memberView struct {
	Subject              string `json:"subject"`
	Email                string `json:"email"`
	Name                 string `json:"name"`
	Role                 string `json:"role"`
	LastLoginAt          string `json:"lastLoginAt"`
	LastSeenAt           string `json:"lastSeenAt"`
	Online               bool   `json:"online"`
	ActiveSessions       int    `json:"activeSessions"`
	TokenCount           int    `json:"tokenCount"`
	Device               string `json:"device"`
	DeviceLeaseExpiresAt string `json:"deviceLeaseExpiresAt"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// adminUsers is administrator-only. Device assignments come from a private
// agent endpoint; an unreachable or older agent only blanks the device columns.
func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !identity.IsAdmin(r.Context()) {
		respond(w, 403, map[string]string{"error": "administrator role required"})
		return
	}
	s.mu.Lock()
	list := s.members
	s.mu.Unlock()
	if list == nil {
		respond(w, 503, map[string]string{"error": "user registry unavailable"})
		return
	}
	type held struct {
		device  string
		expires time.Time
	}
	holders := make(map[string]held)
	for _, result := range s.collect(r, "/api/admin/leases") {
		var body struct {
			Leases []struct {
				Subject   string    `json:"subject"`
				Serial    string    `json:"serial"`
				ExpiresAt time.Time `json:"expiresAt"`
			} `json:"leases"`
		}
		if result.err != nil || json.Unmarshal(result.body, &body) != nil {
			continue
		}
		for _, lease := range body.Leases {
			if current, ok := holders[lease.Subject]; lease.Subject != "" && lease.Serial != "" && (!ok || lease.ExpiresAt.After(current.expires)) {
				holders[lease.Subject] = held{DeviceID(result.id, lease.Serial), lease.ExpiresAt}
			}
		}
	}
	members := list()
	users := make([]memberView, 0, len(members))
	for _, m := range members {
		view := memberView{Subject: m.Subject, Email: m.Email, Name: m.Name, Role: m.Role, LastLoginAt: stamp(m.LastLoginAt), LastSeenAt: stamp(m.LastSeenAt), Online: m.Online, ActiveSessions: m.ActiveSessions, TokenCount: m.TokenCount}
		if h, ok := holders[m.Subject]; ok {
			view.Device, view.DeviceLeaseExpiresAt = h.device, stamp(h.expires)
		}
		users = append(users, view)
	}
	respond(w, 200, map[string]any{"users": users})
}
