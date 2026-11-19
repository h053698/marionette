// Package backend authenticates agents and relays public device APIs to them.
// It has no dependency on ADB, Android tooling, or frontend assets.
package backend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"marionette/client"
	"marionette/internal/agent"
	"marionette/internal/auth"
	"marionette/internal/identity"
	"marionette/internal/service"
	"marionette/internal/tunnel"
)

type Server struct {
	mu         sync.Mutex
	ownership  ownershipState
	video      videoLimits
	poolKey    [32]byte
	tokens     map[string]string
	peers      map[string]*peer
	closed     bool
	closedDone chan struct{}
	leases     service.LeasePolicy
	mux        *http.ServeMux
	next       uint64
	jobs       *jobs
	registry   map[string]agentRecord
	agentFile  string
	lastSeen   map[string]time.Time
	members    func() []auth.Member
}

type peer struct {
	id        string
	session   *yamux.Session
	transport *http.Transport
}

func New(tokens map[string]string) (*Server, error) {
	return NewWithLeasePolicy(tokens, service.LeasePolicy{DefaultTTL: service.DefaultLeaseTTL, MaxTTL: service.DefaultMaxLease})
}

func NewWithLeasePolicy(tokens map[string]string, leases service.LeasePolicy) (*Server, error) {
	s := &Server{tokens: make(map[string]string), peers: make(map[string]*peer), mux: http.NewServeMux(), registry: make(map[string]agentRecord), lastSeen: make(map[string]time.Time), closedDone: make(chan struct{}), leases: leases}
	s.ownership.gates = make(map[string]*subjectGate)
	s.ownership.known = make(map[string]map[string][]rememberedLease)
	s.ownership.uncertain = make(map[string]pendingClaim)
	if err := s.video.configure(); err != nil {
		return nil, err
	}
	if _, err := rand.Read(s.poolKey[:]); err != nil {
		return nil, err
	}
	for id, token := range tokens {
		if !agent.ValidID(id) || len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
			return nil, errors.New("MARIONETTE_AGENT_TOKENS requires valid agent IDs and tokens of at least 32 bytes without whitespace")
		}
		hash := sha256.Sum256([]byte(token))
		s.tokens[id] = hex.EncodeToString(hash[:])
		s.registry[id] = agentRecord{ID: id, Name: id, Hash: s.tokens[id]}
	}
	s.mux.Handle("/mcp", s.MCPHandler())
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.HandleFunc("GET /api/devices", s.devices)
	s.mux.HandleFunc("GET /api/pool", s.pool)
	s.mux.HandleFunc("GET /api/agents", s.agents)
	s.mux.HandleFunc("GET /api/admin/users", s.adminUsers)
	s.mux.HandleFunc("POST /api/admin/agents", s.registerAgent)
	s.mux.HandleFunc("PUT /api/admin/agents/{id}/config", s.configureAgent)
	s.mux.HandleFunc("POST /api/device/occupy", s.occupy)
	s.mux.HandleFunc("POST /api/devices/occupy", s.occupy)
	s.mux.HandleFunc("POST /api/actions/{action}", s.action)
	s.mux.HandleFunc("POST /api/jobs", s.submitJob)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.getJob)
	s.mux.HandleFunc("GET /api/jobs/{id}/artifacts/{index}", s.jobArtifact)
	for _, prefix := range []string{"/api/devices/", "/api/admin/devices/"} {
		s.mux.HandleFunc(prefix+"{device}/{operation}", s.proxy)
	}
	return s, nil
}

// ResourceLimitLogAttrs reports the limits configured before the HTTP server starts.
func (s *Server) ResourceLimitLogAttrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("video_max_global", s.video.maxTotal),
		slog.Int("video_max_per_account", s.video.maxAccount),
	}
	if s.jobs != nil {
		attrs = append(attrs, slog.Duration("job_retention", s.jobs.retention),
			slog.Int64("job_artifact_max_bytes_per_account", s.jobs.artifactLimit))
	}
	return attrs
}

// Connect is mounted outside browser auth: agents use distinct per-PC secrets.
func (s *Server) Connect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	expected := s.tokens[id]
	s.mu.Unlock()
	provided := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if expected == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(provided[:])), []byte(expected)) != 1 || r.Header.Get("Origin") != "" {
		respond(w, 401, map[string]string{"error": "invalid agent credentials"})
		return
	}
	if !contains(websocket.Subprotocols(r), tunnel.Protocol) {
		respond(w, 400, map[string]string{"error": "agent protocol required"})
		return
	}
	s.mu.Lock()
	if s.closed || s.peers[id] != nil && !s.peers[id].session.IsClosed() {
		s.mu.Unlock()
		respond(w, 409, map[string]string{"error": "agent already connected or backend closing"})
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{tunnel.Protocol}}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.mu.Unlock()
		return
	}
	session, err := yamux.Client(tunnel.New(ws), tunnel.Config())
	if err != nil {
		s.mu.Unlock()
		ws.Close()
		return
	}
	p := &peer{id: id, session: session}
	p.transport = &http.Transport{DialContext: p.dial, DisableKeepAlives: true, MaxConnsPerHost: 128, ResponseHeaderTimeout: 30 * time.Second}
	s.peers[id] = p
	s.lastSeen[id] = time.Now()
	s.mu.Unlock()
	defer session.Close()
	<-session.CloseChan()
	p.transport.CloseIdleConnections()
	s.mu.Lock()
	if s.peers[id] == p {
		delete(s.peers, id)
		s.lastSeen[id] = time.Now()
	}
	s.mu.Unlock()
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (p *peer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	ready := make(chan result)
	go func() {
		conn, err := p.session.OpenStream()
		select {
		case ready <- result{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	select {
	case result := <-ready:
		return result.conn, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.closedDone
		return
	}
	s.closed = true
	peers := make([]*peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	defer close(s.closedDone)
	if s.jobs != nil {
		s.jobs.cancel()
		s.jobs.mu.Lock()
		for _, cancel := range s.jobs.active {
			cancel()
		}
		s.jobs.mu.Unlock()
		s.jobs.wg.Wait()
		s.jobs.producer.Close()
	}
	for _, p := range peers {
		p.session.Close()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := identity.FromContext(r.Context()); !ok {
		respond(w, 401, map[string]string{"error": "sign-in required"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	write, _ := json.Marshal(body)
	w.WriteHeader(status)
	_, _ = w.Write(append(write, '\n'))
}

func forwardIdentity(r *http.Request, user identity.User) {
	data, _ := json.Marshal(user)
	r.Header.Set(agent.IdentityHeader, base64.RawURLEncoding.EncodeToString(data))
	for _, name := range []string{"Authorization", "Cookie", "X-CSRF-Token", "Origin", "Referer"} {
		r.Header.Del(name)
	}
}

// DeviceID is stable and keeps identical ADB serials on different PCs distinct.
func DeviceID(agentID, serial string) string {
	return agentID + "." + base64.RawURLEncoding.EncodeToString([]byte(serial))
}

func splitDeviceID(device string) (string, string, bool) {
	id, encoded, ok := strings.Cut(device, ".")
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	serial := string(decoded)
	if !ok || err != nil || !agent.ValidID(id) || serial == "" || strings.ContainsAny(serial, "/\\\x00\r\n") || serial == "." || serial == ".." || DeviceID(id, serial) != device {
		return "", "", false
	}
	return id, serial, true
}

func deviceRoute(serial, operation string, adminPath bool) string {
	if adminPath {
		return "/api/admin/devices/" + serial + "/" + operation
	}
	return "/api/devices/" + serial + "/" + operation
}

func writeDeviceJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	user, _ := identity.FromContext(r.Context())
	admin := identity.IsAdmin(r.Context())
	adminPath := strings.HasPrefix(r.URL.Path, "/api/admin/")
	if adminPath && !admin {
		respond(w, 403, map[string]string{"error": "administrator role required"})
		return
	}
	target, operation := r.PathValue("device"), r.PathValue("operation")
	if operation == "lease" && (r.Method == "POST" || r.Method == "PUT" && !adminPath || r.Method == "DELETE") || operation == "attach" && !adminPath && r.Method == "POST" {
		s.relayLease(w, r, user, adminPath)
		return
	}
	var assignment client.Device
	if !admin {
		var ok bool
		assignment, ok = s.checkDeviceAccess(w, r, user, target)
		if !ok {
			return
		}
	}
	id, serial, ok := splitDeviceID(target)
	if !ok {
		respond(w, 400, map[string]string{"error": "invalid device ID; use serial from /api/devices"})
		return
	}
	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil || p.session.IsClosed() {
		respond(w, 503, map[string]string{"error": "device agent is offline"})
		return
	}
	if (operation == "video" && !adminPath || operation == "preview" && adminPath) && websocket.IsWebSocketUpgrade(r) {
		var lease string
		if assignment.LeaseID != "" {
			lease = target + "/" + assignment.LeaseID
		}
		release, ok := s.video.reserve(user.Subject, lease)
		if !ok {
			respond(w, 429, map[string]string{"code": "video_limit", "error": "too many active video streams"})
			return
		}
		// ReverseProxy stays in ServeHTTP for the lifetime of an upgraded stream.
		defer release()
	}
	path := deviceRoute(serial, operation, adminPath)
	proxy := &httputil.ReverseProxy{
		Transport: p.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "localhost"})
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			forwardIdentity(pr.Out, user)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			respond(w, 502, map[string]string{"error": "agent request failed"})
		},
	}
	proxy.ServeHTTP(w, r)
}

type snapshot struct {
	id   string
	body json.RawMessage
	err  error
}

func (s *Server) collect(r *http.Request, path string) []snapshot {
	s.mu.Lock()
	peers := make([]*peer, 0, len(s.peers))
	for _, p := range s.peers {
		if !p.session.IsClosed() {
			peers = append(peers, p)
		}
	}
	s.mu.Unlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].id < peers[j].id })
	results := make([]snapshot, len(peers))
	user, _ := identity.FromContext(r.Context())
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost"+path, nil)
			forwardIdentity(request, user)
			response, err := p.transport.RoundTrip(request)
			result := snapshot{id: p.id, err: err}
			if err == nil {
				defer response.Body.Close()
				result.body, result.err = io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
				if response.StatusCode != 200 || len(result.body) > 4<<20 || !json.Valid(result.body) {
					result.err = errors.New("invalid agent response")
				}
			}
			results[i] = result
		}()
	}
	wg.Wait()
	return results
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	user, _ := identity.FromContext(r.Context())
	if !identity.IsAdmin(r.Context()) {
		devices, err := s.assignments(r.Context(), user)
		if err != nil && devices == nil {
			respond(w, 503, map[string]string{"error": err.Error()})
			return
		}
		// Retain the partial member inventory behavior while verifying ownership
		// separately for claims and access.
		devices = slices.DeleteFunc(devices, func(device client.Device) bool { return !device.Mine })
		respond(w, 200, map[string]any{"devices": devices})
		return
	}
	devices := make([]map[string]any, 0)
	unavailable := make([]string, 0)
	for _, result := range s.collect(r, "/api/devices") {
		var body struct {
			Devices []map[string]any `json:"devices"`
		}
		if result.err != nil || json.Unmarshal(result.body, &body) != nil {
			unavailable = append(unavailable, result.id)
			continue
		}
		for _, device := range body.Devices {
			serial, _ := device["serial"].(string)
			if serial == "" {
				continue
			}
			device["rawSerial"], device["agentId"] = serial, result.id
			device["serial"] = DeviceID(result.id, serial)
			devices = append(devices, device)
		}
	}
	respond(w, 200, map[string]any{"devices": devices, "unavailableAgents": unavailable})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if user, ok := identity.FromContext(r.Context()); ok && user.Role != "admin" {
		respond(w, 200, map[string]any{"connected": true})
		return
	}
	agents := make([]map[string]any, 0)
	adb, jar := true, true
	for _, result := range s.collect(r, "/api/status") {
		var status struct {
			ADB bool `json:"adbAvailable"`
			Jar bool `json:"deviceJarAvailable"`
		}
		healthy := result.err == nil && json.Unmarshal(result.body, &status) == nil
		agents = append(agents, map[string]any{"id": result.id, "connected": healthy, "adbAvailable": healthy && status.ADB, "deviceJarAvailable": healthy && status.Jar})
		adb = adb && healthy && status.ADB
		jar = jar && healthy && status.Jar
	}
	respond(w, 200, map[string]any{"version": "1", "agentCount": len(agents), "agents": agents, "adbAvailable": len(agents) > 0 && adb, "deviceJarAvailable": len(agents) > 0 && jar})
}
