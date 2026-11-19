package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"marionette/internal/identity"
	"marionette/internal/service"
)

// MCPAuthorization carries authenticated lifetimes independently of the HTTP
// request, since the SDK can dispatch JSON-RPC work after that request ends.
type MCPAuthorization struct {
	Credential context.Context
	Grant      context.Context
	GrantID    string
	Expires    time.Time
	ProxyHost  bool
}

type mcpAuthorizationKey struct{}

func MCPAuthorizationFromContext(ctx context.Context) (MCPAuthorization, bool) {
	a, ok := ctx.Value(mcpAuthorizationKey{}).(MCPAuthorization)
	return a, ok
}

// serveMCP authenticates requests for the hosted MCP endpoint. Clients present an
// OAuth access token issued by this server (or a personal API token). On success
// the downstream handler sees the person's identity with the user role, and the
// request context ends as soon as the grant or token is revoked.
func (s *Service) serveMCP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	public, err := url.Parse(s.origin)
	if err != nil || public.Hostname() == "" || r.Host == "" || !strings.EqualFold(r.Host, public.Host) {
		failure(w, 403, "request host rejected")
		return
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		origin := origins[0]
		if len(origins) != 1 || origin == "" || (!strings.EqualFold(origin, s.origin) && !s.frontend(origin)) {
			failure(w, 403, "request origin rejected")
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	}
	if r.Method == http.MethodOptions {
		h.Add("Vary", "Access-Control-Request-Method")
		h.Add("Vary", "Access-Control-Request-Headers")
		h.Set("Allow", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID, Mcp-Method, Mcp-Name")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var v *session
	a := MCPAuthorization{ProxyHost: !service.Loopback(public.Hostname())}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		switch {
		case strings.HasPrefix(parts[1], oauthAccessStart):
			if access := s.lookupOAuthToken(parts[1]); access != nil {
				v = access.session
				a.Grant, a.GrantID = access.grant, "oauth:"+access.family
			}
		case strings.HasPrefix(parts[1], "mnt_"):
			if v = s.lookupToken(r); v != nil {
				a.Grant, a.GrantID = v.ctx, "pat:"+tokenHash(parts[1])
			}
		}
	}
	if v == nil || v.ctx.Err() != nil {
		code := ""
		if r.Header.Get("Authorization") != "" {
			code = "invalid_token"
		}
		s.challenge(w, code)
		failure(w, 401, "authorization required")
		return
	}
	a.Credential, a.Expires = v.ctx, v.expires
	s.touchUser(v, true)
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(v.ctx, cancel)
	defer stop()
	defer cancel()
	if v.ctx.Err() != nil {
		s.challenge(w, "invalid_token")
		failure(w, 401, "authorization required")
		return
	}
	user := v.user
	user.Role = "user"
	ctx = context.WithValue(identity.WithUser(ctx, user), mcpAuthorizationKey{}, a)
	next.ServeHTTP(w, r.WithContext(ctx))
}
