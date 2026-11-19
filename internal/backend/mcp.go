package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"marionette/client"
	marionetteauth "marionette/internal/auth"
	"marionette/internal/identity"
	"marionette/internal/mcpbridge"
)

const (
	maxMCPUsers           = 1024
	maxMCPSessions        = 1024
	maxMCPSessionsPerUser = 8
	mcpSessionTimeout     = 30 * time.Minute
	mcpSessionHeader      = "Mcp-Session-Id"
	mcpAuthorizationExtra = "marionette"
)

// userBridge is one person's MCP server. Leases stay inside it, so the model
// never sees a lease token and every call runs with that person's identity.
type userBridge struct {
	server   *mcp.Server
	bridge   *mcpbridge.Bridge
	used     time.Time
	sessions int
}

type mcpHost struct {
	server   *Server
	mu       sync.Mutex
	bridges  map[string]*userBridge
	sessions int
}

type mcpPendingSession struct {
	host     *mcpHost
	bridge   *userBridge
	attached bool // guarded by host.mu
	released bool // guarded by host.mu
}

type mcpPendingSessionKey struct{}

type mcpRequestAuthorization struct {
	marionetteauth.MCPAuthorization
	pending *mcpPendingSession
}

type mcpDeviceAuthorization struct{ credential context.Context }
type mcpDeviceAuthorizationKey struct{}

// inProcess serves the client's requests directly through the backend with a
// fixed identity, so the hosted MCP endpoint needs no credentials of its own.
type inProcess struct {
	server *Server
	user   identity.User
}

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	a, ok := r.Context().Value(mcpDeviceAuthorizationKey{}).(mcpDeviceAuthorization)
	if ok && a.credential.Err() != nil {
		return nil, context.Canceled
	}
	// The bridge may release a lease after a failed driver start or at
	// eviction. These server-owned requests still need an owned lease token.
	if !ok && !isMCPLeaseRelease(r) {
		return nil, context.Canceled
	}
	r = r.Clone(identity.WithUser(r.Context(), t.user))
	recorder := httptest.NewRecorder()
	t.server.ServeHTTP(recorder, r)
	response := recorder.Result()
	response.Request = r
	return response, nil
}

func isMCPLeaseRelease(r *http.Request) bool {
	if r.Method != http.MethodDelete || r.Header.Get("X-Lease-Token") == "" || r.URL.RawQuery != "" || r.Body != nil && r.Body != http.NoBody {
		return false
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api/devices/")
	serial, okSuffix := strings.CutSuffix(path, "/lease")
	return ok && okSuffix && serial != "" && !strings.Contains(serial, "/")
}

// MCPHandler serves Streamable HTTP with credentials verified by the auth layer.
func (s *Server) MCPHandler() http.Handler {
	host := &mcpHost{server: s, bridges: make(map[string]*userBridge)}
	verify := func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		a, ok := marionetteauth.MCPAuthorizationFromContext(ctx)
		if !ok || a.Credential.Err() != nil {
			return nil, auth.ErrInvalidToken
		}
		pending, _ := ctx.Value(mcpPendingSessionKey{}).(*mcpPendingSession)
		// The SDK compares UserID on every request to an existing session. The
		// verified grant ID also lets a refreshed OAuth access token resume it.
		return &auth.TokenInfo{UserID: a.GrantID, Expiration: a.Expires, Extra: map[string]any{
			mcpAuthorizationExtra: mcpRequestAuthorization{MCPAuthorization: a, pending: pending},
		}}, nil
	}
	requireBearer := auth.RequireBearerToken(verify, nil)
	local := requireBearer(mcp.NewStreamableHTTPHandler(mcpServerFor, &mcp.StreamableHTTPOptions{
		SessionTimeout: mcpSessionTimeout,
	}))
	proxy := requireBearer(mcp.NewStreamableHTTPHandler(mcpServerFor, &mcp.StreamableHTTPOptions{
		SessionTimeout: mcpSessionTimeout, DisableLocalhostProtection: true,
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := marionetteauth.MCPAuthorizationFromContext(r.Context())
		user, signedIn := identity.FromContext(r.Context())
		if !ok || !signedIn || user.Subject == "" || a.Credential.Err() != nil {
			http.Error(w, "authorization required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get(mcpSessionHeader) == "" {
			pending := host.reserve(user)
			if pending == nil {
				http.Error(w, "MCP session limit reached", http.StatusTooManyRequests)
				return
			}
			defer pending.release(false)
			r = r.WithContext(context.WithValue(r.Context(), mcpPendingSessionKey{}, pending))
		}
		// Only the auth layer can mark a request whose non-loopback canonical
		// Host and Origin were validated. Keep the SDK check for loopback.
		if a.ProxyHost {
			proxy.ServeHTTP(w, r)
			return
		}
		local.ServeHTTP(w, r)
	})
}

func mcpServerFor(r *http.Request) *mcp.Server {
	pending, _ := r.Context().Value(mcpPendingSessionKey{}).(*mcpPendingSession)
	if pending == nil {
		return nil
	}
	return pending.bridge.server
}

func (h *mcpHost) reserve(user identity.User) *mcpPendingSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions >= maxMCPSessions {
		return nil
	}
	b := h.bridges[user.Subject]
	if b == nil {
		if len(h.bridges) >= maxMCPUsers && !h.evictIdleBridgeLocked() {
			return nil
		}
		user.Role = "user"
		api := client.NewWithTransport("http://marionette.internal", inProcess{server: h.server, user: user})
		server, bridge := mcpbridge.New(api)
		server.AddReceivingMiddleware(checkMCPAuthorization)
		b = &userBridge{server: server, bridge: bridge}
		h.bridges[user.Subject] = b
	}
	if b.sessions >= maxMCPSessionsPerUser {
		return nil
	}
	b.sessions++
	h.sessions++
	return &mcpPendingSession{host: h, bridge: b}
}

func (h *mcpHost) evictIdleBridgeLocked() bool {
	var oldest *userBridge
	var subject string
	for id, b := range h.bridges {
		if b.sessions == 0 && (oldest == nil || b.used.Before(oldest.used)) {
			oldest, subject = b, id
		}
	}
	if oldest == nil {
		return false
	}
	// Server-owned cleanup releases only the idle bridge's leases.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = oldest.bridge.Close(ctx)
	delete(h.bridges, subject)
	return true
}

// An initial HTTP POST reserves capacity before the SDK creates a session.
// An initialize message transfers the reservation to the SDK session's lifetime.
func (p *mcpPendingSession) attach(ss *mcp.ServerSession, grant context.Context) bool {
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if p.released || p.attached || grant.Err() != nil {
		return false
	}
	p.attached = true
	stop := context.AfterFunc(grant, func() { _ = ss.Close() })
	go func() {
		_ = ss.Wait()
		_ = ss.Close() // Ensure the SDK has removed the transport session.
		stop()
		p.release(true)
	}()
	return true
}

func (p *mcpPendingSession) release(closed bool) {
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if !p.released && (closed || !p.attached) {
		p.released = true
		p.bridge.sessions--
		p.bridge.used = time.Now()
		p.host.sessions--
	}
}

func checkMCPAuthorization(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "initialize" && method != "tools/call" {
			return next(ctx, method, req)
		}
		var a mcpRequestAuthorization
		if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
			a, _ = extra.TokenInfo.Extra[mcpAuthorizationExtra].(mcpRequestAuthorization)
		}
		if a.Credential == nil || a.Grant == nil || a.Credential.Err() != nil {
			return nil, errors.New("MCP credential expired or revoked")
		}
		if method == "initialize" {
			ss, ok := req.GetSession().(*mcp.ServerSession)
			if !ok || a.pending == nil || !a.pending.attach(ss, a.Grant) {
				return nil, errors.New("MCP session initialization rejected")
			}
			return next(ctx, method, req)
		}
		// The SDK detaches JSON-RPC work from the HTTP request. Use the exact
		// POST credential so canceling one access token also cancels its tools.
		ctx, cancel := context.WithDeadline(ctx, a.Expires)
		defer cancel()
		stop := context.AfterFunc(a.Credential, cancel)
		defer stop()
		if a.Credential.Err() != nil {
			return nil, errors.New("MCP credential expired or revoked")
		}
		ctx = context.WithValue(ctx, mcpDeviceAuthorizationKey{}, mcpDeviceAuthorization{credential: a.Credential})
		result, err := next(ctx, method, req)
		if a.Credential.Err() != nil {
			return nil, errors.New("MCP credential expired or revoked")
		}
		return result, err
	}
}
