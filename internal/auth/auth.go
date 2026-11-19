// Package auth implements an OIDC relying party. Sessions and login transactions
// are bounded, in-memory state: restarting the backend logs every browser out.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"marionette/internal/identity"
	"marionette/internal/service"
)

type Config struct {
	// PublicOrigin is the canonical API origin and supplies the default callback.
	PublicOrigin                                  string
	IssuerURL, ClientID, ClientSecret, RedirectURL string
	AdminEmails, AdminSubjects                     []string
	// FrontendOrigins lists browser origins (scheme://host[:port]) hosted apart from
	// this backend. They may call the API with credentials and receive CORS headers.
	// The first entry is where the browser returns after sign-in.
	FrontendOrigins []string
	SessionTTL      time.Duration
	OAuthAccessTTL  time.Duration
	OAuthRefreshTTL time.Duration
	OAuthCodeTTL    time.Duration
	// TokenFile persists personal API token hashes. Empty keeps tokens in memory.
	TokenFile string
	// UserFile persists the registry of known users (identity and timestamps only).
	// Empty keeps it in memory.
	UserFile string
	// OAuthFile persists OAuth client registrations and refresh-token hashes for
	// the hosted MCP endpoint. Empty keeps them in memory.
	OAuthFile string
}

const (
	DefaultSessionTTL = 8 * time.Hour
	MinSessionTTL     = time.Minute
	MaxSessionTTL     = 24 * time.Hour
	pendingTTL  = 5 * time.Minute
	maxPending  = 1024
	maxSessions = 4096
)

type transaction struct {
	binding, nonce, verifier string
	next                     string // local path to resume after sign-in (OAuth consent)
	expires                  time.Time
}
type session struct {
	user     identity.User
	verified bool // the identity provider verified user.Email
	started  time.Time
	csrf     string
	expires  time.Time
	ctx      context.Context
	cancel   context.CancelFunc
}
type Service struct {
	configured       bool
	oauth            oauth2.Config
	verifier         *oidc.IDTokenVerifier
	origin           string
	secure           bool
	crossSite        bool            // session cookie must be SameSite=None
	frontends        map[string]bool // lowercase allowed frontend origins
	landing          string          // post-login redirect target
	ttl              time.Duration
	oauthAccessTTL    time.Duration
	oauthRefreshTTL   time.Duration
	oauthCodeTTL      time.Duration
	admins, subjects map[string]bool
	client           *http.Client
	mu               sync.Mutex
	pending          map[string]transaction
	loginRequests    ipBuckets
	sessions         map[string]*session
	tokens           map[string]*apiToken // SHA-256 digest -> token; never the bearer secret
	tokenFile        string
	usersMu          sync.Mutex // guards users; never held together with mu
	userSaveMu       sync.Mutex // serializes registry writes; taken before usersMu
	users            map[string]*knownUser
	userFile         string
	grants           *oauthState // OAuth 2.1 authorization server for the hosted MCP endpoint
	usersDirty       bool
	usersFlushed     time.Time
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

// New requires the issuer, client ID, secret and an explicit or derived callback
// when OIDC is enabled.
// SessionTTL defaults to eight hours and must be between one minute and 24 hours.
func New(ctx context.Context, cfg Config) (*Service, error) {
	s := &Service{pending: make(map[string]transaction), sessions: make(map[string]*session), admins: make(map[string]bool), subjects: make(map[string]bool), frontends: make(map[string]bool), tokens: make(map[string]*apiToken), tokenFile: cfg.TokenFile, users: make(map[string]*knownUser), userFile: cfg.UserFile, grants: newOAuthState(cfg.OAuthFile)}
	s.ttl = cfg.SessionTTL
	if s.ttl == 0 {
		s.ttl = DefaultSessionTTL
	}
	if s.ttl < MinSessionTTL || s.ttl > MaxSessionTTL {
		return nil, errors.New("session TTL must be between one minute and 24 hours")
	}
	s.oauthAccessTTL, s.oauthRefreshTTL, s.oauthCodeTTL = cfg.OAuthAccessTTL, cfg.OAuthRefreshTTL, cfg.OAuthCodeTTL
	if s.oauthAccessTTL == 0 {
		s.oauthAccessTTL = DefaultOAuthAccessTTL
	}
	if s.oauthRefreshTTL == 0 {
		s.oauthRefreshTTL = DefaultOAuthRefreshTTL
	}
	if s.oauthCodeTTL == 0 {
		s.oauthCodeTTL = DefaultOAuthCodeTTL
	}
	for _, ttl := range []time.Duration{s.oauthAccessTTL, s.oauthRefreshTTL, s.oauthCodeTTL} {
		if ttl < time.Second || ttl%time.Second != 0 {
			return nil, errors.New("OAuth lifetimes must be positive whole-second durations")
		}
	}
	for i, raw := range cfg.FrontendOrigins {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		origin, err := service.Origin(raw)
		if err != nil {
			return nil, fmt.Errorf("MARIONETTE_FRONTEND_ORIGINS entry %d %w", i+1, err)
		}
		origin = strings.ToLower(origin)
		s.frontends[origin] = true
		if s.landing == "" {
			s.landing = origin + "/"
		}
	}
	for i, email := range cfg.AdminEmails {
		if email = strings.TrimSpace(email); email != "" {
			address, err := mail.ParseAddress(email)
			if err != nil || address.Address != email {
				return nil, fmt.Errorf("MARIONETTE_ADMIN_EMAILS entry %d must be an email address", i+1)
			}
			s.admins[strings.ToLower(email)] = true
		}
	}
	for _, sub := range cfg.AdminSubjects {
		if sub != "" {
			s.subjects[sub] = true
		}
	}
	var public *url.URL
	if cfg.PublicOrigin != "" {
		origin, err := service.Origin(cfg.PublicOrigin)
		if err != nil {
			return nil, fmt.Errorf("MARIONETTE_PUBLIC_URL %w", err)
		}
		cfg.PublicOrigin = origin
		public, _ = url.Parse(origin)
	}
	anyConfig := cfg.IssuerURL != "" || cfg.ClientID != "" || cfg.ClientSecret != "" || cfg.RedirectURL != ""
	if cfg.RedirectURL == "" && public != nil && (anyConfig || !service.Loopback(public.Hostname())) {
		cfg.RedirectURL = cfg.PublicOrigin + "/auth/callback"
		anyConfig = true
	}
	if anyConfig {
		if strings.TrimSpace(cfg.ClientID) == "" || cfg.ClientSecret == "" || cfg.IssuerURL == "" || cfg.RedirectURL == "" {
			return nil, errors.New("OIDC requires MARIONETTE_OIDC_ISSUER, MARIONETTE_OIDC_CLIENT_ID, MARIONETTE_OIDC_CLIENT_SECRET and either MARIONETTE_OIDC_REDIRECT_URL or MARIONETTE_PUBLIC_URL")
		}
		if _, e := service.HTTPURL(cfg.IssuerURL); e != nil {
			return nil, fmt.Errorf("MARIONETTE_OIDC_ISSUER %w", e)
		}
		redirect, e := service.HTTPURL(cfg.RedirectURL)
		if e != nil {
			return nil, fmt.Errorf("MARIONETTE_OIDC_REDIRECT_URL %w", e)
		}
		if redirect.Path != "/auth/callback" || redirect.RawPath != "" {
			return nil, errors.New("MARIONETTE_OIDC_REDIRECT_URL path must be /auth/callback")
		}
		s.origin = redirect.Scheme + "://" + redirect.Host
		if public != nil {
			if public.Scheme != redirect.Scheme || !strings.EqualFold(public.Hostname(), redirect.Hostname()) {
				return nil, errors.New("MARIONETTE_PUBLIC_URL must share the OIDC callback's scheme and hostname")
			}
			s.origin = cfg.PublicOrigin
		}
		if !service.Loopback(redirect.Hostname()) && len(s.admins) == 0 && len(s.subjects) == 0 {
			return nil, errors.New("set MARIONETTE_ADMIN_EMAILS or MARIONETTE_ADMIN_SUBJECTS for a non-loopback public origin")
		}
		s.secure = redirect.Scheme == "https"
		s.client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			_, e := service.HTTPURL(r.URL.String())
			return e
		}}
		discovery, cancel := context.WithTimeout(oidc.ClientContext(ctx, s.client), 15*time.Second)
		defer cancel()
		provider, e := oidc.NewProvider(discovery, cfg.IssuerURL)
		if e != nil {
			return nil, errors.New("OIDC discovery failed")
		}
		endpoint := provider.Endpoint()
		var metadata struct {
			JWKS string `json:"jwks_uri"`
		}
		if e = provider.Claims(&metadata); e != nil {
			return nil, errors.New("invalid OIDC discovery")
		}
		for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKS} {
			if _, e = service.HTTPURL(raw); e != nil {
				return nil, errors.New("unsafe OIDC endpoint")
			}
		}
		s.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL, Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
		s.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
		s.configured = true
	}
	s.crossSite = len(s.frontends) > 0 && s.secure
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.loadTokens(); err != nil {
		s.cancel()
		return nil, err
	}
	if err := s.loadUsers(); err != nil {
		s.cancel()
		return nil, err
	}
	if err := s.loadOAuth(); err != nil {
		s.cancel()
		return nil, err
	}
	s.wg.Add(1)
	go s.cleaner()
	return s, nil
}
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
	s.closeOAuth()
	s.usersMu.Lock()
	dirty := s.usersDirty
	s.usersMu.Unlock()
	if dirty {
		if err := s.flushUsers(); err != nil {
			slog.Warn("could not save user registry", "error", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, v := range s.sessions {
		v.cancel()
		delete(s.sessions, id)
	}
	clear(s.pending)
	for _, token := range s.tokens {
		token.cancel()
	}
	clear(s.tokens)
}

// Configured reports whether OIDC discovery and verifier setup succeeded.
func (s *Service) Configured() bool { return s.configured }
func (s *Service) cleaner() {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			s.pruneLocked()
			s.mu.Unlock()
			s.pruneOAuth()
			s.flushUsersIfDue()
		}
	}
}
func (s *Service) pruneLocked() {
	now := time.Now()
	for id, v := range s.pending {
		if !now.Before(v.expires) {
			delete(s.pending, id)
		}
	}
	for id, v := range s.sessions {
		if !now.Before(v.expires) {
			v.cancel()
			delete(s.sessions, id)
		}
	}
	for hash, token := range s.tokens {
		if !now.Before(token.ExpiresAt) {
			token.cancel()
			delete(s.tokens, hash)
		}
	}
}
func secret() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func (s *Service) cookieName(purpose string) string {
	if s.secure {
		return "__Host-marionette-" + purpose
	}
	return "marionette-" + purpose
}
func (s *Service) cookie(w http.ResponseWriter, purpose, value string, expires time.Time) {
	sameSite := http.SameSiteLaxMode
	if purpose == "session" && s.crossSite {
		sameSite = http.SameSiteNoneMode // a frontend on another site sends it with fetch and WebSocket
	}
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(purpose), Value: value, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: sameSite, Expires: expires, MaxAge: maxAge})
}
func (s *Service) lookup(r *http.Request) (string, *session) {
	c, e := r.Cookie(s.cookieName("session"))
	if e != nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[c.Value]
	if v != nil && (!time.Now().Before(v.expires) || v.ctx.Err() != nil) {
		v.cancel()
		delete(s.sessions, c.Value)
		v = nil
	}
	return c.Value, v
}
func unsafe(r *http.Request) bool {
	return r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
}
func (s *Service) originOK(r *http.Request) bool {
	if s.configured {
		u, _ := url.Parse(s.origin)
		if !strings.EqualFold(r.Host, u.Host) {
			if r.URL.Path != "/auth/callback" {
				return false
			}
			// The configured callback can use a separate port on the same host.
			callback, _ := url.Parse(s.oauth.RedirectURL)
			if !strings.EqualFold(r.Host, callback.Host) {
				return false
			}
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !s.configured {
			return s.frontend(origin)
		}
		u, e := url.Parse(origin)
		if e != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		if s.frontend(origin) {
			return true
		}
		if !strings.EqualFold(u.Scheme+"://"+u.Host, s.origin) {
			return false
		}
	}
	navigation := r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	return r.URL.Path == "/auth/callback" || (r.URL.Path == "/auth/login" && (len(s.frontends) > 0 || navigation)) || r.Header.Get("Sec-Fetch-Site") != "cross-site"
}

// frontend reports whether origin is a configured separate frontend.
func (s *Service) frontend(origin string) bool {
	return origin != "" && s.frontends[strings.ToLower(origin)]
}

// cors answers preflight requests and marks responses to a configured frontend.
// It returns true when the request is finished.
func (s *Service) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if !s.frontend(origin) {
		return false
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Add("Vary", "Access-Control-Request-Method")
		h.Add("Vary", "Access-Control-Request-Headers")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, X-Lease-Token")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func (s *Service) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.oauthPublic(w, r) {
			return
		}
		if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
			s.serveMCP(w, r, next)
			return
		}
		if s.cors(w, r) {
			return
		}
		authPath := strings.HasPrefix(r.URL.Path, "/auth/")
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if authPath || api {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
		}
		if (authPath || api || unsafe(r) || strings.EqualFold(r.Header.Get("Upgrade"), "websocket")) && !s.originOK(r) {
			failure(w, 403, "request origin rejected")
			return
		}
		id, v := s.lookup(r)
		bearer := r.Header.Get("Authorization") != ""
		if bearer {
			// An invalid explicit credential never falls back to a browser cookie.
			v = s.lookupToken(r)
			if v == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="marionette"`)
				failure(w, 401, "invalid or expired API token")
				return
			}
			if !api || r.URL.Path == "/api/tokens" || strings.HasPrefix(r.URL.Path, "/api/tokens/") || strings.HasPrefix(r.URL.Path, "/api/oauth/") {
				failure(w, 403, "browser sign-in required for token management")
				return
			}
		}
		if v != nil {
			s.touchUser(v, bearer)
		}
		if r.URL.Path == "/api/session" && r.Method == "GET" {
			body := map[string]any{"authenticated": v != nil, "configured": s.configured, "loginURL": "/auth/login"}
			if v != nil {
				body["user"] = v.user
				if !bearer {
					body["csrfToken"] = v.csrf
				}
				body["expiresAt"] = v.expires
			}
			respond(w, 200, body)
			return
		}
		if r.URL.Path == "/oauth/authorize" {
			w.Header().Set("Referrer-Policy", "no-referrer")
			s.oauthAuthorize(w, r, v)
			return
		}
		if r.URL.Path == "/auth/login" && r.Method == "GET" {
			s.login(w, r)
			return
		}
		if r.URL.Path == "/auth/callback" && r.Method == "GET" {
			s.callback(w, r)
			return
		}
		if authPath && !(r.URL.Path == "/auth/logout" && r.Method == "POST") {
			failure(w, 404, "unknown authentication endpoint")
			return
		}
		if (api || authPath || (r.Method != "GET" && r.Method != "HEAD")) && v == nil {
			if !s.configured {
				failure(w, 503, "sign-in is not configured")
			} else {
				failure(w, 401, "sign-in required")
			}
			return
		}
		if v != nil && !bearer && unsafe(r) && !equal(r.Header.Get("X-CSRF-Token"), v.csrf) {
			failure(w, 403, "invalid CSRF token")
			return
		}
		if r.URL.Path == "/auth/logout" {
			s.mu.Lock()
			if current := s.sessions[id]; current != nil {
				current.cancel()
				delete(s.sessions, id)
			}
			s.mu.Unlock()
			s.cookie(w, "session", "", time.Unix(1, 0))
			respond(w, 200, map[string]bool{"ok": true})
			return
		}
		if v != nil {
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(v.ctx, cancel)
			defer stop()
			defer cancel()
			if v.ctx.Err() != nil {
				cancel()
			}
			r = r.WithContext(identity.WithUser(ctx, v.user))
		}
		if r.URL.Path == "/api/tokens" || strings.HasPrefix(r.URL.Path, "/api/tokens/") {
			s.manageTokens(w, r, v.user)
			return
		}
		if r.URL.Path == "/api/oauth/grants" || strings.HasPrefix(r.URL.Path, "/api/oauth/grants/") {
			s.manageGrants(w, r, v.user)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	if !s.configured {
		failure(w, 503, "sign-in is not configured")
		return
	}
	var tx transaction
	if next := r.URL.Query().Get("next"); strings.HasPrefix(next, "/oauth/authorize?") && len(next) <= 4096 && !strings.ContainsAny(next, "\r\n\\") {
		tx.next = next
	}
	s.mu.Lock()
	s.pruneLocked()
	if len(s.pending) >= maxPending || s.ctx.Err() != nil {
		s.mu.Unlock()
		failure(w, 503, "sign-in temporarily unavailable")
		return
	}
	now := time.Now()
	if !s.loginRequests.allow(r, now, 12, 15*time.Second) {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "15")
		failure(w, http.StatusTooManyRequests, "too many sign-in requests")
		return
	}
	if binding, err := r.Cookie(s.cookieName("login")); err == nil {
		for id, old := range s.pending {
			if equal(binding.Value, old.binding) {
				tx.binding = old.binding
				delete(s.pending, id)
				break
			}
		}
	}
	if tx.binding == "" {
		tx.binding = secret()
	}
	state := secret()
	tx.nonce, tx.verifier, tx.expires = secret(), oauth2.GenerateVerifier(), now.Add(pendingTTL)
	s.pending[state] = tx
	s.mu.Unlock()
	s.cookie(w, "login", tx.binding, tx.expires)
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(tx.verifier), oidc.Nonce(tx.nonce)), http.StatusFound)
}
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	if !s.configured {
		failure(w, 503, "sign-in is not configured")
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	binding, e := r.Cookie(s.cookieName("login"))
	if e != nil || len(q["state"]) != 1 {
		failure(w, 400, "invalid sign-in transaction")
		return
	}
	s.mu.Lock()
	tx, ok := s.pending[state]
	valid := ok && time.Now().Before(tx.expires) && equal(binding.Value, tx.binding)
	if valid || ok && !time.Now().Before(tx.expires) {
		delete(s.pending, state)
	}
	s.mu.Unlock()
	if !valid {
		failure(w, 400, "invalid sign-in transaction")
		return
	}
	s.cookie(w, "login", "", time.Unix(1, 0))
	if q.Get("error") != "" || len(q["code"]) != 1 || q.Get("code") == "" {
		failure(w, 400, "sign-in was not completed")
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.client), 15*time.Second)
	defer cancel()
	token, e := s.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(tx.verifier))
	if e != nil {
		failure(w, 401, "sign-in token exchange failed")
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		failure(w, 401, "identity token missing")
		return
	}
	verified, e := s.verifier.Verify(ctx, raw)
	if e != nil || verified.Subject == "" || !equal(verified.Nonce, tx.nonce) {
		failure(w, 401, "identity token rejected")
		return
	}
	var claims struct {
		Email           string `json:"email"`
		Verified        bool   `json:"email_verified"`
		Name            string `json:"name"`
		AuthorizedParty string `json:"azp"`
	}
	if e = verified.Claims(&claims); e != nil || (claims.AuthorizedParty != "" && claims.AuthorizedParty != s.oauth.ClientID) || (len(verified.Audience) > 1 && claims.AuthorizedParty != s.oauth.ClientID) {
		failure(w, 401, "identity claims rejected")
		return
	}
	user := identity.User{Subject: verified.Subject, Email: claims.Email, Name: claims.Name, Role: s.roleFor(verified.Subject, claims.Email, claims.Verified)}
	now := time.Now()
	expiry := now.Add(s.ttl)
	sessionCtx, sessionCancel := context.WithDeadline(s.ctx, expiry)
	v := &session{user: user, verified: claims.Verified, started: now, csrf: secret(), expires: expiry, ctx: sessionCtx, cancel: sessionCancel}
	id := secret()
	s.mu.Lock()
	s.pruneLocked()
	if len(s.sessions) >= maxSessions || s.ctx.Err() != nil {
		s.mu.Unlock()
		sessionCancel()
		failure(w, 503, "sign-in temporarily unavailable")
		return
	}
	if old, e := r.Cookie(s.cookieName("session")); e == nil {
		if previous := s.sessions[old.Value]; previous != nil {
			previous.cancel()
			delete(s.sessions, old.Value)
		}
	}
	s.sessions[id] = v
	s.mu.Unlock()
	s.recordLogin(user, claims.Verified, now)
	s.cookie(w, "session", id, expiry)
	landing := "/"
	if s.landing != "" {
		landing = s.landing
	}
	if tx.next != "" {
		landing = tx.next // the consent page is served by this backend
	}
	http.Redirect(w, r, landing, http.StatusFound)
}
