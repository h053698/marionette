package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"marionette/internal/identity"
)

// The hosted MCP endpoint (/mcp) is an OAuth 2.1 protected resource and this
// service is its authorization server. Clients register dynamically as public
// clients (RFC 7591), authenticate with PKCE (S256), and the person signs in
// through the same OIDC session as the web console and approves once. Access
// tokens are short lived and memory only; refresh tokens rotate and persist
// as hashes. Every OAuth credential acts with role "user" like a personal token.

const (
	oauthScope             = "mcp"
	DefaultOAuthAccessTTL  = time.Hour
	DefaultOAuthRefreshTTL = 30 * 24 * time.Hour
	DefaultOAuthCodeTTL    = 5 * time.Minute
	maxOAuthClients        = 512
	maxOAuthRefresh        = 4096
	maxOAuthCodes          = 1024
	maxOAuthAccess         = 4096
	maxOAuthBody           = 16 << 10
	maxOAuthStore          = 8 << 20
	maxOAuthClientMetadata = 4 << 10
	oauthConsentTTL        = 10 * time.Minute
	oauthReuseTTL          = 10 * time.Minute
	oauthAccessStart       = "mat_"
)

type oauthClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirectUris"`
	CreatedAt    time.Time `json:"createdAt"`
}

type oauthRefresh struct {
	Hash       string        `json:"hash"`
	Family     string        `json:"family"`
	ClientID   string        `json:"clientId"`
	User       identity.User `json:"user"`
	CreatedAt  time.Time     `json:"createdAt"`
	LastUsedAt time.Time     `json:"lastUsedAt"`
	ExpiresAt  time.Time     `json:"expiresAt"`
	Used       bool          `json:"used"`
}

type oauthCode struct {
	clientID, redirect, challenge string
	user                          identity.User
	expires                       time.Time
}

type oauthAccess struct {
	family, clientID string
	session          *session
	grant            context.Context
}

type oauthGrant struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type oauthConsent struct {
	params  authzParams
	csrf    string
	expires time.Time
}

type oauthState struct {
	mu            sync.Mutex
	file          string
	clients       map[string]*oauthClient
	refresh       map[string]*oauthRefresh
	codes         map[string]oauthCode
	consents      map[string]oauthConsent
	access        map[string]*oauthAccess
	families      map[string]oauthGrant
	registrations ipBuckets
	dirty         bool
}

func newOAuthState(file string) *oauthState {
	return &oauthState{
		file: file, clients: make(map[string]*oauthClient), refresh: make(map[string]*oauthRefresh),
		codes: make(map[string]oauthCode), consents: make(map[string]oauthConsent),
		access: make(map[string]*oauthAccess), families: make(map[string]oauthGrant),
	}
}

type ipBucket struct {
	tokens float64
	at     time.Time
}

// ipBuckets is guarded by its caller's mutex. Only the direct peer is trusted;
// an untrusted Forwarded or X-Forwarded-For header cannot select a new bucket.
type ipBuckets map[string]ipBucket

func (b *ipBuckets) allow(r *http.Request, now time.Time, burst int, refill time.Duration) bool {
	key := "unknown"
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			key = ip.String()
		}
	}
	if *b == nil {
		*b = make(ipBuckets)
	}
	bucket, ok := (*b)[key]
	if !ok {
		const maxIPBuckets = 4096
		if len(*b) >= maxIPBuckets {
			for ip, old := range *b {
				if now.Sub(old.at) >= time.Duration(burst)*refill {
					delete(*b, ip)
				}
			}
			if len(*b) >= maxIPBuckets {
				return false
			}
		}
		bucket.tokens = float64(burst)
	} else {
		bucket.tokens = min(float64(burst), bucket.tokens+float64(now.Sub(bucket.at))/float64(refill))
	}
	// Preserve the last refill time on rejection so a denied request cannot
	// keep a depleted bucket from eventually being reclaimed.
	if bucket.tokens < 1 {
		return false
	}
	(*b)[key] = ipBucket{tokens: bucket.tokens - 1, at: now}
	return true
}

func oauthHash(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Origin is the canonical API origin used as the OAuth issuer; empty without OIDC.
func (s *Service) Origin() string { return s.origin }

func (s *Service) resourceURL() string { return s.origin + "/mcp" }

func (s *Service) loadOAuth() error {
	o := s.grants
	if o.file == "" {
		return nil
	}
	file, err := os.Open(o.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open OAuth store: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxOAuthStore+1))
	if err != nil {
		return fmt.Errorf("read OAuth store: %w", err)
	}
	if len(data) > maxOAuthStore {
		return errors.New("OAuth store exceeds size limit")
	}
	var stored struct {
		Clients []oauthClient  `json:"clients"`
		Refresh []oauthRefresh `json:"refresh"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("read OAuth store: %w", err)
	}
	if len(stored.Clients) > maxOAuthClients || len(stored.Refresh) > maxOAuthRefresh {
		return errors.New("invalid OAuth store")
	}
	now := time.Now()
	for i := range stored.Clients {
		c := stored.Clients[i]
		if c.ID == "" || len(c.RedirectURIs) == 0 || o.clients[c.ID] != nil {
			return errors.New("invalid OAuth client record")
		}
		for _, uri := range c.RedirectURIs {
			if !validRedirectURI(uri) {
				return errors.New("invalid OAuth client redirect")
			}
		}
		o.clients[c.ID] = &c
	}
	for i := range stored.Refresh {
		r := stored.Refresh[i]
		if r.Hash == "" || r.Family == "" || r.User.Subject == "" || o.refresh[r.Hash] != nil || o.clients[r.ClientID] == nil {
			return errors.New("invalid OAuth refresh record")
		}
		if now.Before(r.ExpiresAt) {
			o.refresh[r.Hash] = &r
		}
	}
	o.pruneRefreshLocked(now)
	return nil
}

var errOAuthStoreLimit = errors.New("OAuth store exceeds capacity")

// saveLocked reports whether rename succeeded, even if the directory sync
// failed. The caller then knows whether a rollback also needs a disk repair.
func (o *oauthState) saveLocked() (renamed bool, err error) {
	if len(o.clients) > maxOAuthClients || len(o.refresh) > maxOAuthRefresh {
		return false, errOAuthStoreLimit
	}
	var stored struct {
		Clients []*oauthClient  `json:"clients"`
		Refresh []*oauthRefresh `json:"refresh"`
	}
	stored.Clients = make([]*oauthClient, 0, len(o.clients))
	for _, c := range o.clients {
		stored.Clients = append(stored.Clients, c)
	}
	stored.Refresh = make([]*oauthRefresh, 0, len(o.refresh))
	for _, r := range o.refresh {
		stored.Refresh = append(stored.Refresh, r)
	}
	sort.Slice(stored.Clients, func(i, j int) bool { return stored.Clients[i].ID < stored.Clients[j].ID })
	sort.Slice(stored.Refresh, func(i, j int) bool { return stored.Refresh[i].Hash < stored.Refresh[j].Hash })
	var data bytes.Buffer
	if err := json.NewEncoder(&data).Encode(stored); err != nil {
		return false, err
	}
	if data.Len() > maxOAuthStore {
		return false, errOAuthStoreLimit
	}
	if o.file == "" {
		return false, nil
	}
	dir := filepath.Dir(o.file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	file, err := os.CreateTemp(dir, ".oauth-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	_, err = data.WriteTo(file)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), o.file); err != nil {
		return false, err
	}
	return true, directory.Sync()
}

func (o *oauthState) commitLocked(rollback func()) error {
	renamed, err := o.saveLocked()
	if err == nil {
		o.dirty = false
		return nil
	}
	rollback()
	if renamed {
		o.dirty = true
		_ = o.flushDirtyLocked()
	}
	if !errors.Is(err, errOAuthStoreLimit) {
		slog.Warn("could not commit OAuth store", "error", err)
	}
	return err
}

func (o *oauthState) flushDirtyLocked() error {
	if !o.dirty {
		return nil
	}
	if _, err := o.saveLocked(); err != nil {
		slog.Warn("could not flush OAuth store; will retry", "error", err)
		return err
	}
	o.dirty = false
	return nil
}

func oauthCommitError(w http.ResponseWriter, err error) {
	if errors.Is(err, errOAuthStoreLimit) {
		oauthError(w, 503, "temporarily_unavailable", "OAuth store is at capacity")
		return
	}
	oauthError(w, 500, "server_error", "could not save the OAuth store")
}

// pruneOAuth drops expired material. Called from the cleaner without s.mu.
func (s *Service) pruneOAuth() {
	o := s.grants
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneAuthorizationsLocked(now)
	o.pruneRefreshLocked(now)
	o.pruneAccessLocked(now)
	if len(o.families) > 0 {
		live := make(map[string]bool)
		for _, r := range o.refresh {
			if !r.Used {
				live[r.Family] = true
			}
		}
		for _, a := range o.access {
			live[a.family] = true
		}
		for family, grant := range o.families {
			if !live[family] {
				grant.cancel()
				delete(o.families, family)
			}
		}
	}
	_ = o.flushDirtyLocked()
}

func (o *oauthState) pruneAuthorizationsLocked(now time.Time) {
	for hash, c := range o.codes {
		if !now.Before(c.expires) {
			delete(o.codes, hash)
		}
	}
	for id, c := range o.consents {
		if !now.Before(c.expires) {
			delete(o.consents, id)
		}
	}
}

func (o *oauthState) pruneAccessLocked(now time.Time) {
	for hash, a := range o.access {
		if !now.Before(a.session.expires) || a.session.ctx.Err() != nil {
			a.session.cancel()
			delete(o.access, hash)
		}
	}
}

func (o *oauthState) pruneRefreshLocked(now time.Time) {
	latest := make(map[string]*oauthRefresh)
	for hash, r := range o.refresh {
		if !now.Before(r.ExpiresAt) || r.Used && !now.Before(r.LastUsedAt.Add(oauthReuseTTL)) {
			delete(o.refresh, hash)
		} else if r.Used {
			if old := latest[r.Family]; old != nil {
				if r.LastUsedAt.Before(old.LastUsedAt) || r.LastUsedAt.Equal(old.LastUsedAt) && r.Hash < old.Hash {
					delete(o.refresh, hash)
					continue
				}
				delete(o.refresh, old.Hash)
			}
			latest[r.Family] = r
		}
	}
}

func (s *Service) closeOAuth() {
	o := s.grants
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.flushDirtyLocked()
	for hash, a := range o.access {
		a.session.cancel()
		delete(o.access, hash)
	}
}

// lookupOAuthToken resolves a bearer access token. The returned session never
// carries admin rights, whatever role the person has in the web console.
func (s *Service) lookupOAuthToken(raw string) *oauthAccess {
	o := s.grants
	o.mu.Lock()
	defer o.mu.Unlock()
	a := o.access[oauthHash(raw)]
	if a == nil || a.session.ctx.Err() != nil || !time.Now().Before(a.session.expires) {
		return nil
	}
	return a
}

func oauthJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	oauthJSON(w, status, map[string]string{"error": code, "error_description": description})
}

var redirectSchemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]{1,31}$`)

// validRedirectURI accepts HTTPS, loopback HTTP and private-use app schemes (RFC 8252).
func validRedirectURI(raw string) bool {
	if raw == "" || len(raw) > 512 || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "\r\n\\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	case "javascript", "data", "file", "vbscript", "about", "blob", "ftp", "ws", "wss":
		return false
	}
	return redirectSchemeRE.MatchString(u.Scheme)
}

func cleanName(raw string, limit int) string {
	raw = clip(strings.TrimSpace(raw), limit)
	var b strings.Builder
	for _, r := range raw {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// oauthPublic serves discovery, registration, token and revocation endpoints.
// They carry no ambient credentials, so any origin may call them.
func (s *Service) oauthPublic(w http.ResponseWriter, r *http.Request) bool {
	if !s.configured {
		return false
	}
	post := false
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp":
	case "/oauth/register", "/oauth/token", "/oauth/revoke":
		post = true
	default:
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	h.Set("Referrer-Policy", "no-referrer")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if post != (r.Method == http.MethodPost) || (!post && r.Method != http.MethodGet) {
		if post {
			h.Set("Allow", "POST, OPTIONS")
		} else {
			h.Set("Allow", "GET, OPTIONS")
		}
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "unsupported method")
		return true
	}
	switch r.URL.Path {
	case "/oauth/register":
		s.oauthRegister(w, r)
	case "/oauth/token":
		s.oauthToken(w, r)
	case "/oauth/revoke":
		s.oauthRevoke(w, r)
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		oauthJSON(w, 200, map[string]any{
			"resource":                 s.resourceURL(),
			"resource_name":            "Marionette device control",
			"authorization_servers":    []string{s.origin},
			"scopes_supported":         []string{oauthScope},
			"bearer_methods_supported": []string{"header"},
		})
	default:
		oauthJSON(w, 200, map[string]any{
			"issuer":                                         s.origin,
			"authorization_endpoint":                         s.origin + "/oauth/authorize",
			"token_endpoint":                                 s.origin + "/oauth/token",
			"registration_endpoint":                          s.origin + "/oauth/register",
			"revocation_endpoint":                            s.origin + "/oauth/revoke",
			"scopes_supported":                               []string{oauthScope},
			"response_types_supported":                       []string{"code"},
			"response_modes_supported":                       []string{"query"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"revocation_endpoint_auth_methods_supported":     []string{"none"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		})
	}
	return true
}

// challenge tells an unauthenticated MCP client where to start the OAuth flow.
func (s *Service) challenge(w http.ResponseWriter, code string) {
	value := `Bearer resource_metadata="` + s.origin + `/.well-known/oauth-protected-resource"`
	if code != "" {
		value += `, error="` + code + `"`
	}
	w.Header().Set("WWW-Authenticate", value)
}

func (s *Service) oauthRegister(w http.ResponseWriter, r *http.Request) {
	o := s.grants
	o.mu.Lock()
	allowed := o.registrations.allow(r, time.Now(), 8, 30*time.Second)
	o.mu.Unlock()
	if !allowed {
		w.Header().Set("Retry-After", "30")
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registration requests")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBody)
	var in struct {
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
	}
	dec := json.NewDecoder(r.Body)
	var extra any
	if dec.Decode(&in) != nil || dec.Decode(&extra) != io.EOF {
		oauthError(w, 400, "invalid_client_metadata", "request must be a JSON client registration")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 8 {
		oauthError(w, 400, "invalid_redirect_uri", "provide 1 to 8 redirect URIs")
		return
	}
	seen := make(map[string]bool)
	for _, uri := range in.RedirectURIs {
		if !validRedirectURI(uri) || seen[uri] {
			oauthError(w, 400, "invalid_redirect_uri", "redirect URIs must be unique HTTPS, loopback HTTP or app-scheme URIs without fragments")
			return
		}
		seen[uri] = true
	}
	if in.AuthMethod != "" && in.AuthMethod != "none" {
		oauthError(w, 400, "invalid_client_metadata", "only public clients (token_endpoint_auth_method none) are supported")
		return
	}
	for _, grant := range in.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			oauthError(w, 400, "invalid_client_metadata", "unsupported grant type")
			return
		}
	}
	for _, kind := range in.ResponseTypes {
		if kind != "code" {
			oauthError(w, 400, "invalid_client_metadata", "unsupported response type")
			return
		}
	}
	name := cleanName(in.ClientName, 100)
	if name == "" {
		name = "MCP 클라이언트"
	}
	now := time.Now()
	client := &oauthClient{ID: "mc_" + secret(), Name: name, RedirectURIs: in.RedirectURIs, CreatedAt: now}
	metadata, err := json.Marshal(client)
	if err != nil || len(metadata) > maxOAuthClientMetadata {
		oauthError(w, 400, "invalid_client_metadata", "client metadata exceeds 4096 bytes")
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneRefreshLocked(now)
	o.pruneAccessLocked(now)
	o.pruneAuthorizationsLocked(now)
	var oldest *oauthClient
	if len(o.clients) >= maxOAuthClients {
		// Registrations with no live grant or authorization may be replaced.
		protected := make(map[string]bool)
		for _, r := range o.refresh {
			protected[r.ClientID] = true
		}
		for _, a := range o.access {
			protected[a.clientID] = true
		}
		for _, code := range o.codes {
			protected[code.clientID] = true
		}
		for _, consent := range o.consents {
			protected[consent.params.ClientID] = true
		}
		for _, c := range o.clients {
			if !protected[c.ID] && (oldest == nil || c.CreatedAt.Before(oldest.CreatedAt)) {
				oldest = c
			}
		}
		if oldest == nil {
			oauthError(w, 503, "temporarily_unavailable", "too many registered clients")
			return
		}
		delete(o.clients, oldest.ID)
	}
	o.clients[client.ID] = client
	if err := o.commitLocked(func() {
		delete(o.clients, client.ID)
		if oldest != nil {
			o.clients[oldest.ID] = oldest
		}
	}); err != nil {
		oauthCommitError(w, err)
		return
	}
	oauthJSON(w, 201, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      oauthScope,
	})
}

type authzParams struct {
	ClientID, RedirectURI, State, Challenge string
}

// Called with o.mu held so that a registration cannot be evicted between
// validation and creation of its pending consent.
func (s *Service) parseAuthzLocked(vals url.Values) (authzParams, *oauthClient, string) {
	p := authzParams{ClientID: vals.Get("client_id"), RedirectURI: vals.Get("redirect_uri"), State: vals.Get("state"), Challenge: vals.Get("code_challenge")}
	client := s.grants.clients[p.ClientID]
	if client == nil {
		return p, nil, "알 수 없는 앱이에요. 앱에서 연결을 다시 시작해 주세요."
	}
	registered := false
	for _, uri := range client.RedirectURIs {
		if subtle.ConstantTimeCompare([]byte(uri), []byte(p.RedirectURI)) == 1 {
			registered = true
		}
	}
	if !registered {
		return p, client, "등록되지 않은 돌아갈 주소예요."
	}
	if len(p.State) > 1024 {
		return p, client, "state 값이 너무 길어요."
	}
	switch {
	case vals.Get("response_type") != "code":
		return p, client, "response_type=code만 지원해요."
	case vals.Get("code_challenge_method") != "S256" || !pkceChallengeRE.MatchString(p.Challenge):
		return p, client, "PKCE S256 검증이 필요해요."
	}
	if resource := vals.Get("resource"); resource != "" && resource != s.resourceURL() {
		return p, client, "resource 값은 " + s.resourceURL() + "이어야 해요."
	}
	for scope := range strings.FieldsSeq(vals.Get("scope")) {
		if scope != oauthScope {
			return p, client, "mcp 범위만 지원해요."
		}
	}
	return p, client, ""
}

var (
	pkceChallengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	pkceVerifierRE  = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
)

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Marionette 연결 승인</title>
<style>
*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;background:#f7f8fa;color:#1a1d23;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Apple SD Gothic Neo","Pretendard","Noto Sans KR",sans-serif}
main{width:min(440px,calc(100% - 32px));background:#fff;border:1px solid #e8eaee;border-radius:14px;padding:28px;box-shadow:0 8px 30px #1a1d230f}
h1{margin:0 0 4px;font-size:19px;letter-spacing:-.01em}p{margin:0 0 14px;color:#3d434d}.muted{color:#7a828f;font-size:13px}
.app{margin:16px 0;padding:12px 14px;background:#f7f8fa;border-radius:10px}.app strong{display:block}.app span{color:#7a828f;font-size:13px;overflow-wrap:anywhere}
ul{margin:0 0 18px;padding-left:18px;color:#3d434d}li{margin:3px 0}
.row{display:flex;gap:8px}button{flex:1;height:42px;border-radius:999px;border:1px solid #d5d9e0;background:#fff;font:inherit;font-weight:600;cursor:pointer;color:#1a1d23}
button.allow{background:#2f6bff;border-color:#2f6bff;color:#fff}button:focus-visible{outline:3px solid #2f6bff55;outline-offset:2px}
</style></head><body><main>
<h1>연결을 승인할까요?</h1>
<p>아래 앱이 내 Marionette 계정으로 기기를 조작하려고 해요.</p>
<div class="app"><strong>{{.Client}}</strong><span>{{.Host}}</span></div>
<ul>
<li>기기 배정 · 반납, 화면 확인, 터치·키·텍스트 입력</li>
<li>계정당 동시에 1대만 쓸 수 있어요</li>
<li>관리자 권한은 전달되지 않아요</li>
</ul>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="consent" value="{{.Consent}}">
<div class="row"><button type="submit" name="decision" value="deny">거절</button><button class="allow" type="submit" name="decision" value="allow" autofocus>승인</button></div>
</form>
<p class="muted" style="margin-top:14px">{{.User}} 계정으로 로그인되어 있어요. 연결은 웹 콘솔의 API / MCP에서 언제든 끊을 수 있어요.</p>
</main></body></html>`))

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>연결할 수 없어요</title>
<style>body{margin:0;min-height:100dvh;display:grid;place-items:center;background:#f7f8fa;color:#1a1d23;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Apple SD Gothic Neo","Noto Sans KR",sans-serif}main{width:min(420px,calc(100% - 32px));background:#fff;border:1px solid #e8eaee;border-radius:14px;padding:28px}h1{margin:0 0 8px;font-size:18px}p{margin:0;color:#3d434d}</style>
</head><body><main><h1>연결할 수 없어요</h1><p>{{.}}</p></main></body></html>`))

func authzPage(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
}

func authzErrorPage(w http.ResponseWriter, status int, description string) {
	authzPage(w, status)
	_ = errorTemplate.Execute(w, description)
}

func authzRedirect(w http.ResponseWriter, r *http.Request, issuer string, p authzParams, values map[string]string) {
	u, err := url.Parse(p.RedirectURI)
	if err != nil {
		failure(w, 400, "invalid redirect")
		return
	}
	q := u.Query()
	for k, v := range values {
		q.Set(k, v)
	}
	if p.State != "" {
		q.Set("state", p.State)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// oauthAuthorize shows the consent page to a signed-in person and, once approved,
// returns a single-use authorization code to the client's registered redirect.
func (s *Service) oauthAuthorize(w http.ResponseWriter, r *http.Request, v *session) {
	if !s.configured {
		failure(w, 404, "OAuth is not configured")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		failure(w, 405, "unsupported method")
		return
	}
	o := s.grants
	now := time.Now()
	if r.Method == http.MethodGet {
		o.mu.Lock()
		p, client, issue := s.parseAuthzLocked(r.URL.Query())
		if issue != "" {
			o.mu.Unlock()
			authzErrorPage(w, 400, issue)
			return
		}
		if v == nil {
			o.mu.Unlock()
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		o.pruneAuthorizationsLocked(now)
		if len(o.consents) >= maxOAuthCodes {
			o.mu.Unlock()
			authzErrorPage(w, 503, "진행 중인 연결 요청이 너무 많아요. 잠시 뒤 다시 시도해 주세요.")
			return
		}
		id := secret()
		o.consents[id] = oauthConsent{params: p, csrf: v.csrf, expires: minTime(now.Add(oauthConsentTTL), v.expires)}
		o.mu.Unlock()
		redirect, _ := url.Parse(p.RedirectURI)
		host := redirect.Host
		if host == "" {
			host = redirect.Scheme + "://"
		}
		authzPage(w, 200)
		_ = consentTemplate.Execute(w, map[string]string{
			"Client": client.Name, "Host": host, "CSRF": v.csrf, "Consent": id, "User": firstNonEmpty(v.user.Email, v.user.Name, v.user.Subject),
		})
		return
	}
	if v == nil {
		authzErrorPage(w, 401, "먼저 로그인해 주세요.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBody)
	if err := r.ParseForm(); err != nil {
		authzErrorPage(w, 400, "올바르지 않은 승인 요청이에요.")
		return
	}
	if !equal(r.PostForm.Get("csrf"), v.csrf) || v.ctx.Err() != nil {
		authzErrorPage(w, 403, "승인 요청을 확인할 수 없어요. 앱에서 연결을 다시 시작해 주세요.")
		return
	}
	decision := r.PostForm.Get("decision")
	if decision != "allow" && decision != "deny" {
		authzErrorPage(w, 400, "허용 또는 거절을 선택해 주세요.")
		return
	}
	o.mu.Lock()
	now = time.Now()
	o.pruneAuthorizationsLocked(now)
	id := r.PostForm.Get("consent")
	consent, ok := o.consents[id]
	if !ok || !equal(consent.csrf, v.csrf) || v.ctx.Err() != nil || !now.Before(v.expires) {
		o.mu.Unlock()
		authzErrorPage(w, 400, "승인 요청이 만료됐어요. 앱에서 연결을 다시 시작해 주세요.")
		return
	}
	p := consent.params
	if decision == "deny" {
		delete(o.consents, id)
		o.mu.Unlock()
		authzRedirect(w, r, s.origin, p, map[string]string{"error": "access_denied", "error_description": "the user denied the request"})
		return
	}
	if len(o.codes) >= maxOAuthCodes {
		o.mu.Unlock()
		authzErrorPage(w, 503, "진행 중인 연결 요청이 너무 많아요. 잠시 뒤 다시 시도해 주세요.")
		return
	}
	code := secret()
	user := v.user
	user.Role = "user"
	delete(o.consents, id)
	o.codes[oauthHash(code)] = oauthCode{clientID: p.ClientID, redirect: p.RedirectURI, challenge: p.Challenge, user: user, expires: now.Add(s.oauthCodeTTL)}
	o.mu.Unlock()
	authzRedirect(w, r, s.origin, p, map[string]string{"code": code})
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (s *Service) oauthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "request must be a form")
		return
	}
	form := r.PostForm
	o := s.grants
	clientID := form.Get("client_id")
	if resource := form.Get("resource"); resource != "" && resource != s.resourceURL() {
		oauthError(w, 400, "invalid_target", "resource must be "+s.resourceURL())
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	o.pruneRefreshLocked(now)
	o.pruneAccessLocked(now)
	client := o.clients[clientID]
	if client == nil {
		oauthError(w, 401, "invalid_client", "unknown client; register again")
		return
	}
	var user identity.User
	var family string
	var previous *oauthRefresh
	var codeHash string
	var code oauthCode
	switch form.Get("grant_type") {
	case "authorization_code":
		codeHash = oauthHash(form.Get("code"))
		var ok bool
		code, ok = o.codes[codeHash]
		delete(o.codes, codeHash) // Single use, except when the server cannot commit.
		verifier := form.Get("code_verifier")
		if !ok || !now.Before(code.expires) || code.clientID != clientID || code.redirect != form.Get("redirect_uri") || !pkceVerifierRE.MatchString(verifier) {
			oauthError(w, 400, "invalid_grant", "the authorization code is invalid or expired")
			return
		}
		digest := sha256.Sum256([]byte(verifier))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(digest[:])), []byte(code.challenge)) != 1 {
			oauthError(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		user, family = code.user, secret()
	case "refresh_token":
		hash := oauthHash(form.Get("refresh_token"))
		record := o.refresh[hash]
		if record == nil || record.ClientID != clientID {
			oauthError(w, 400, "invalid_grant", "the refresh token is invalid or expired")
			return
		}
		if record.Used {
			// A rotated token came back: assume it leaked and end the whole grant.
			o.revokeFamilyLocked(record.Family, "")
			_ = o.flushDirtyLocked()
			oauthError(w, 400, "invalid_grant", "the refresh token was already used")
			return
		}
		user, family, previous = record.User, record.Family, record
	default:
		oauthError(w, 400, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	restoreCode := func() {
		if codeHash != "" {
			o.codes[codeHash] = code
		}
	}
	if len(o.access) >= maxOAuthAccess || s.ctx.Err() != nil {
		restoreCode()
		oauthError(w, 503, "temporarily_unavailable", "too many active grants")
		return
	}
	user.Role = "user"
	accessRaw := oauthAccessStart + secret()
	refreshRaw := "mrt_" + secret()
	refreshHash := oauthHash(refreshRaw)
	created := now
	var older *oauthRefresh
	if previous != nil {
		for hash, r := range o.refresh {
			if r.Family == family && r.Used {
				older = r
				delete(o.refresh, hash)
				break
			}
		}
		used := *previous
		used.Used = true
		used.LastUsedAt = now
		o.refresh[used.Hash] = &used
		created = previous.CreatedAt
	}
	o.refresh[refreshHash] = &oauthRefresh{Hash: refreshHash, Family: family, ClientID: clientID, User: user, CreatedAt: created, LastUsedAt: now, ExpiresAt: now.Add(s.oauthRefreshTTL)}
	if err := o.commitLocked(func() {
		delete(o.refresh, refreshHash)
		if previous != nil {
			o.refresh[previous.Hash] = previous
		}
		if older != nil {
			o.refresh[older.Hash] = older
		}
		restoreCode()
	}); err != nil {
		oauthCommitError(w, err)
		return
	}
	grant, ok := o.families[family]
	if !ok {
		grant.ctx, grant.cancel = context.WithCancel(s.ctx)
		o.families[family] = grant
	}
	accessExpiry := now.Add(s.oauthAccessTTL)
	ctx, cancel := context.WithDeadline(grant.ctx, accessExpiry)
	o.access[oauthHash(accessRaw)] = &oauthAccess{family: family, clientID: clientID, grant: grant.ctx, session: &session{user: user, started: now, expires: accessExpiry, ctx: ctx, cancel: cancel}}
	oauthJSON(w, 200, map[string]any{
		"access_token":  accessRaw,
		"token_type":    "Bearer",
		"expires_in":    int(s.oauthAccessTTL / time.Second),
		"refresh_token": refreshRaw,
		"scope":         oauthScope,
	})
}

// revokeFamilyLocked ends a grant; subject limits it to one person when set.
func (o *oauthState) revokeFamilyLocked(family, subject string) bool {
	found := false
	for hash, r := range o.refresh {
		if r.Family == family && (subject == "" || r.User.Subject == subject) {
			delete(o.refresh, hash)
			o.dirty = true
			found = true
		}
	}
	for hash, a := range o.access {
		if a.family == family && (subject == "" || a.session.user.Subject == subject) {
			a.session.cancel()
			delete(o.access, hash)
			found = true
		}
	}
	if found {
		if grant, ok := o.families[family]; ok {
			grant.cancel()
			delete(o.families, family)
		}
	}
	return found
}

func (s *Service) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "request must be a form")
		return
	}
	hash := oauthHash(r.PostForm.Get("token"))
	o := s.grants
	o.mu.Lock()
	defer o.mu.Unlock()
	// Public clients prove nothing, but the token itself is the bearer secret.
	if record := o.refresh[hash]; record != nil {
		o.revokeFamilyLocked(record.Family, "")
	} else if access := o.access[hash]; access != nil {
		access.session.cancel()
		delete(o.access, hash)
	}
	if err := o.flushDirtyLocked(); err != nil {
		oauthError(w, 503, "temporarily_unavailable", "revocation could not be saved yet")
		return
	}
	oauthJSON(w, 200, map[string]bool{})
}

type grantInfo struct {
	ID         string    `json:"id"`
	Client     string    `json:"client"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// manageGrants lets a signed-in person list and revoke the apps connected through OAuth.
func (s *Service) manageGrants(w http.ResponseWriter, r *http.Request, user identity.User) {
	w.Header().Set("Cache-Control", "no-store")
	o := s.grants
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	switch {
	case r.URL.Path == "/api/oauth/grants" && r.Method == http.MethodGet:
		items := make([]grantInfo, 0)
		for _, record := range o.refresh {
			if record.User.Subject != user.Subject || record.Used || !now.Before(record.ExpiresAt) {
				continue
			}
			name := "알 수 없는 앱"
			if client := o.clients[record.ClientID]; client != nil {
				name = client.Name
			}
			items = append(items, grantInfo{ID: record.Family, Client: name, CreatedAt: record.CreatedAt, LastUsedAt: record.LastUsedAt, ExpiresAt: record.ExpiresAt})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		respond(w, 200, map[string]any{"grants": items})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/oauth/grants/"):
		if !o.revokeFamilyLocked(strings.TrimPrefix(r.URL.Path, "/api/oauth/grants/"), user.Subject) {
			failure(w, 404, "unknown connected app")
			return
		}
		if err := o.flushDirtyLocked(); err != nil {
			failure(w, 500, "revocation could not be saved yet")
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		failure(w, 405, "unsupported operation")
	}
}
