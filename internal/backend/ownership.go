package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"marionette/client"
	"marionette/internal/identity"
)

type pendingClaim struct {
	Device string
	Until  time.Time
}

type rememberedLease struct {
	device, id string
	expiresAt  time.Time
}

type subjectGate struct {
	held chan struct{}
	refs int
}

type ownershipState struct {
	mu        sync.Mutex
	gates     map[string]*subjectGate
	known     map[string]map[string][]rememberedLease // subject -> agent -> leases
	uncertain map[string]pendingClaim
	nextSweep time.Time
}

const (
	leaseRPCDeadline  = 15 * time.Second
	leaseReadDeadline = 5 * time.Second
	maxLeaseJSON      = 64 << 10
	deviceUnavailable = "device not available"
)

var errAssignments = errors.New("could not verify existing device assignments")
var errAlreadyAssigned = errors.New("이미 기기를 하나 사용 중이에요. 먼저 반납해 주세요.")

func (o *ownershipState) lock(ctx context.Context, subject string) (func(), error) {
	o.mu.Lock()
	gate := o.gates[subject]
	if gate == nil {
		gate = &subjectGate{held: make(chan struct{}, 1)}
		o.gates[subject] = gate
	}
	gate.refs++
	o.mu.Unlock()
	unref := func() {
		o.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(o.gates, subject)
		}
		o.mu.Unlock()
	}
	select {
	case gate.held <- struct{}{}:
		unlock := func() { <-gate.held; unref() }
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

func (o *ownershipState) remember(subject, agent string, leases []rememberedLease) {
	o.mu.Lock()
	defer o.mu.Unlock()
	byAgent := o.known[subject]
	if len(leases) == 0 {
		delete(byAgent, agent)
		if len(byAgent) == 0 {
			delete(o.known, subject)
		}
		return
	}
	if byAgent == nil {
		byAgent = make(map[string][]rememberedLease)
		o.known[subject] = byAgent
	}
	byAgent[agent] = leases
}

// sweep drops expired guard entries for accounts that may never call again.
// Call with mu held. Every conflict check also checks expiry directly.
func (o *ownershipState) sweep(now time.Time) {
	if now.Before(o.nextSweep) {
		return
	}
	o.nextSweep = now.Add(time.Minute)
	for subject, pending := range o.uncertain {
		if !now.Before(pending.Until) {
			delete(o.uncertain, subject)
		}
	}
	for subject, byAgent := range o.known {
		for agent, leases := range byAgent {
			remaining := leases[:0]
			for _, lease := range leases {
				if now.Before(lease.expiresAt) {
					remaining = append(remaining, lease)
				}
			}
			if len(remaining) == 0 {
				delete(byAgent, agent)
			} else {
				byAgent[agent] = remaining
			}
		}
		if len(byAgent) == 0 {
			delete(o.known, subject)
		}
	}
}

// owned returns live assignments. Successful snapshots replace remembered leases
// for that agent only. Call under the subject gate to order reads and writes.
func (s *Server) owned(ctx context.Context, user identity.User) ([]client.Device, error) {
	r, _ := http.NewRequestWithContext(identity.WithUser(ctx, user), "GET", "http://localhost/api/assignments", nil)
	devices := make([]client.Device, 0)
	var lookupErr error
	for _, result := range s.collect(r, "/api/assignments") {
		var body struct {
			Devices []client.Device `json:"devices"`
		}
		if result.err != nil || json.Unmarshal(result.body, &body) != nil || body.Devices == nil {
			lookupErr = errAssignments
			continue
		}
		leases := make([]rememberedLease, 0, len(body.Devices))
		valid := true
		for i := range body.Devices {
			device := &body.Devices[i]
			if !device.Mine || device.Serial == "" || device.Occupied && (device.LeaseID == "" || device.ExpiresAt == nil || device.ExpiresAt.IsZero()) {
				valid = false
				break
			}
			device.RawSerial, device.AgentID = device.Serial, result.id
			device.Serial = DeviceID(result.id, device.Serial)
			if device.LeaseID != "" && device.ExpiresAt != nil {
				leases = append(leases, rememberedLease{device.Serial, device.LeaseID, *device.ExpiresAt})
			}
		}
		if !valid {
			lookupErr = errAssignments
			continue
		}
		s.ownership.remember(user.Subject, result.id, leases)
		devices = append(devices, body.Devices...)
	}
	return devices, lookupErr
}

func (s *Server) assignments(ctx context.Context, user identity.User) ([]client.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, leaseRPCDeadline)
	defer cancel()
	unlock, err := s.ownership.lock(ctx, user.Subject)
	if err != nil {
		return nil, errAssignments
	}
	defer unlock()
	return s.owned(ctx, user)
}

func (s *Server) mayClaim(ctx context.Context, user identity.User, target string) (int, error) {
	devices, err := s.owned(ctx, user)
	if err != nil {
		return 503, err
	}
	o := &s.ownership
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	o.sweep(now)
	if pending, ok := o.uncertain[user.Subject]; ok && now.Before(pending.Until) && pending.Device != target {
		return 409, errors.New("previous device assignment is unresolved; return that device or wait for its lease to expire")
	}
	for _, leases := range o.known[user.Subject] {
		for _, lease := range leases {
			if lease.device != target && now.Before(lease.expiresAt) {
				return 409, errAlreadyAssigned
			}
		}
	}
	for _, device := range devices {
		if device.Serial != target {
			return 409, errAlreadyAssigned
		}
	}
	return 200, nil
}

func findAssignment(devices []client.Device, target string) (client.Device, bool) {
	for _, device := range devices {
		if device.Serial == target {
			return device, true
		}
	}
	return client.Device{}, false
}

func (s *Server) checkDeviceAccess(w http.ResponseWriter, r *http.Request, user identity.User, target string) (client.Device, bool) {
	devices, err := s.assignments(r.Context(), user)
	if err != nil {
		respond(w, 503, map[string]string{"error": err.Error()})
		return client.Device{}, false
	}
	device, ok := findAssignment(devices, target)
	if !ok {
		respond(w, 403, map[string]string{"error": deviceUnavailable})
	}
	return device, ok
}

func (s *Server) relayLease(w http.ResponseWriter, r *http.Request, user identity.User, adminPath bool) {
	operation := r.PathValue("operation")
	data, status, err := s.readLeaseRequest(w, r, operation, adminPath)
	if err != nil {
		respond(w, status, map[string]string{"error": err.Error()})
		return
	}
	data, status, err = s.mutateLease(r.Context(), user, r.PathValue("device"), r.Method, operation, r.Header.Get("X-Lease-Token"), data, adminPath, r.URL.RawQuery)
	if err != nil {
		respond(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeDeviceJSON(w, status, data)
}

func (s *Server) readLeaseRequest(w http.ResponseWriter, r *http.Request, operation string, adminPath bool) ([]byte, int, error) {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(leaseReadDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, 500, errors.New("could not set lease body read deadline")
	}
	limit := int64(maxLeaseJSON)
	if operation == "attach" {
		limit = 4096 // Match the host's handoff code endpoint.
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var networkError net.Error
		switch {
		case errors.As(err, &tooLarge):
			return nil, 413, errors.New("lease request body too large")
		case errors.As(err, &networkError) && networkError.Timeout():
			return nil, 408, errors.New("lease request body timed out")
		default:
			return nil, 400, errors.New("could not read lease request body")
		}
	}
	_ = controller.SetReadDeadline(time.Time{})
	if r.Method == http.MethodDelete && !adminPath {
		var release struct {
			Wipe bool `json:"wipe"`
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && (trimmed[0] != '{' || json.Unmarshal(trimmed, &release) != nil) {
			return nil, 400, errors.New("invalid release JSON")
		}
		return data, 200, nil
	}
	var input struct {
		Code    string `json:"code"`
		Owner   string `json:"owner"`
		TTL     int    `json:"ttlSeconds"`
		Resume  *bool  `json:"resume"`
		LeaseID string `json:"leaseId"`
	}
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) || json.Unmarshal(data, &input) != nil {
		return nil, 400, errors.New("invalid lease JSON")
	}
	if operation == "attach" {
		if len(input.Code) != 48 {
			return nil, 400, errors.New("invalid connection code")
		}
	} else {
		if adminPath && input.LeaseID == "" {
			return nil, 400, errors.New("current leaseId is required")
		}
		if _, err := s.leases.Duration(input.TTL); err != nil {
			return nil, 400, err
		}
	}
	return data, 200, nil
}

func (s *Server) mutateLease(ctx context.Context, user identity.User, device, method, operation, token string, body []byte, adminPath bool, rawQuery string) ([]byte, int, error) {
	agentID, serial, ok := splitDeviceID(device)
	if !ok {
		return nil, 400, errors.New("invalid device ID")
	}
	device = DeviceID(agentID, serial)
	ctx, cancel := context.WithTimeout(ctx, leaseRPCDeadline)
	defer cancel()
	unlock, err := s.ownership.lock(ctx, user.Subject)
	if err != nil {
		return nil, 503, errAssignments
	}
	defer unlock()
	if method == http.MethodPost {
		if status, err := s.mayClaim(ctx, user, device); err != nil {
			return nil, status, err
		}
	} else if method != http.MethodDelete && user.Role != "admin" {
		devices, err := s.owned(ctx, user)
		if err != nil {
			return nil, 503, err
		}
		if _, ok := findAssignment(devices, device); !ok {
			return nil, 403, errors.New(deviceUnavailable)
		}
	}
	var headers http.Header
	data, status, err := s.agentRequestWithQuery(ctx, user, agentID, serial, method, operation, token, body, adminPath, maxLeaseJSON, rawQuery, &headers)
	if method == http.MethodDelete {
		if err == nil && status >= 200 && status < 300 && !adminPath {
			if id := headers.Get("X-Marionette-Released-Lease-ID"); id != "" {
				// A retried release may have succeeded before a newer lease began.
				s.ownership.forget(user.Subject, agentID, device, id)
			}
			s.ownership.clearPending(user.Subject, device)
		}
		return data, status, err
	}
	if err == nil && status >= 200 && status < 300 {
		var lease client.Lease
		if json.Unmarshal(data, &lease) == nil && lease.ID != "" && lease.Token != "" && !lease.ExpiresAt.IsZero() {
			s.ownership.remember(user.Subject, agentID, []rememberedLease{{device, lease.ID, lease.ExpiresAt}})
			s.ownership.clearPending(user.Subject, device)
			return data, status, nil
		}
		status, err = 502, errors.New("invalid lease response")
	}
	if status >= 500 {
		// The agent may have committed even though the response was lost.
		s.ownership.mu.Lock()
		now := time.Now()
		s.ownership.sweep(now)
		if pending, exists := s.ownership.uncertain[user.Subject]; !exists || !now.Before(pending.Until) || pending.Device == device {
			s.ownership.uncertain[user.Subject] = pendingClaim{Device: device, Until: now.Add(s.leases.MaxTTL)}
		}
		s.ownership.mu.Unlock()
	}
	return data, status, err
}

func (o *ownershipState) clearPending(subject, device string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.uncertain[subject].Device == device {
		delete(o.uncertain, subject)
	}
}

func (s *Server) claimDevice(ctx context.Context, user identity.User, device string, ttl int) ([]byte, int, error) {
	if _, err := s.leases.Duration(ttl); err != nil {
		return nil, 400, err
	}
	// Automatic allocation requires a fresh lease, even for the same device.
	body, _ := json.Marshal(map[string]any{"ttlSeconds": ttl, "resume": false})
	data, status, err := s.mutateLease(ctx, user, device, http.MethodPost, "lease", "", body, false, "")
	return deviceResult(data, status, err)
}

func (s *Server) releaseDevice(ctx context.Context, user identity.User, device string, lease client.Lease) error {
	agentID, serial, ok := splitDeviceID(device)
	if !ok {
		return errors.New("invalid device ID")
	}
	unlock, err := s.ownership.lock(ctx, user.Subject)
	if err != nil {
		return err
	}
	defer unlock()
	// The host checks the exact lease token and subject. Unrelated agent
	// lookups must never prevent cleanup of a lease the backend already holds.
	if _, _, err := deviceResult(s.agentRequest(ctx, user, agentID, serial, http.MethodDelete, "lease", lease.Token, nil, false, maxLeaseJSON)); err != nil {
		return err
	}
	s.ownership.forget(user.Subject, agentID, device, lease.ID)
	return nil
}

func (o *ownershipState) forget(subject, agent, device, id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	byAgent := o.known[subject]
	leases := byAgent[agent]
	remaining := leases[:0]
	for _, lease := range leases {
		if lease.device != device || (id != "" && lease.id != id) {
			remaining = append(remaining, lease)
		}
	}
	if len(remaining) == 0 {
		delete(byAgent, agent)
	} else {
		byAgent[agent] = remaining
	}
	if len(byAgent) == 0 {
		delete(o.known, subject)
	}
}
