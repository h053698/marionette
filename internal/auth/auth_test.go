package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"marionette/internal/identity"
)

type testProvider struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	codes     map[string]url.Values
	overrides map[string]any
	corrupt   bool
	exchanges int
}

func provider(t *testing.T) *testProvider {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	p := &testProvider{key: key, codes: map[string]url.Values{}, overrides: map[string]any{}}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			respond(w, 200, map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			respond(w, 200, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		case "/authorize":
			q := r.URL.Query()
			if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("nonce") == "" || q.Get("state") == "" || q.Get("response_type") != "code" || q.Get("scope") != "openid profile email" {
				http.Error(w, "invalid authorization request", 400)
				return
			}
			code := secret()
			p.mu.Lock()
			p.codes[code] = q
			p.mu.Unlock()
			dest, _ := url.Parse(q.Get("redirect_uri"))
			values := dest.Query()
			values.Set("code", code)
			values.Set("state", q.Get("state"))
			dest.RawQuery = values.Encode()
			http.Redirect(w, r, dest.String(), 302)
		case "/token":
			_ = r.ParseForm()
			p.mu.Lock()
			q, ok := p.codes[r.Form.Get("code")]
			delete(p.codes, r.Form.Get("code"))
			p.exchanges++
			p.mu.Unlock()
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			id, password, basic := r.BasicAuth()
			if !ok || !basic || id != "client" || password != "secret" || base64.RawURLEncoding.EncodeToString(digest[:]) != q.Get("code_challenge") || r.Form.Get("redirect_uri") != q.Get("redirect_uri") {
				respond(w, 400, map[string]string{"error": "invalid_grant"})
				return
			}
			claims := map[string]any{"iss": p.server.URL, "sub": "subject-1", "aud": "client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce"), "email": "Admin@example.com", "email_verified": true, "name": "Test User"}
			p.mu.Lock()
			for k, v := range p.overrides {
				claims[k] = v
			}
			corrupt := p.corrupt
			p.mu.Unlock()
			header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
			body, _ := json.Marshal(claims)
			unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
			hash := sha256.Sum256([]byte(unsigned))
			signature, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
			if e != nil {
				t.Error(e)
				return
			}
			if corrupt {
				signature[0] ^= 1
			}
			respond(w, 200, map[string]any{"access_token": "server-only-access-token", "token_type": "Bearer", "id_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

type harness struct {
	service  *Service
	provider *testProvider
	server   *httptest.Server
	client   *http.Client
}

func setup(t *testing.T, adjust func(*Config), next http.Handler) *harness {
	t.Helper()
	p := provider(t)
	mux := http.NewServeMux()
	rp := httptest.NewServer(mux)
	t.Cleanup(rp.Close)
	cfg := Config{IssuerURL: p.server.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: rp.URL + "/auth/callback", AdminEmails: []string{"admin@example.com"}}
	if adjust != nil {
		adjust(&cfg)
	}
	s, e := New(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	if next == nil {
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, _ := identity.FromContext(r.Context())
			respond(w, 200, user)
		})
	}
	mux.Handle("/", s.Wrap(next))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	return &harness{s, p, rp, client}
}
func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	if strings.HasPrefix(path, "/") {
		path = h.server.URL + path
	}
	r, e := h.client.Get(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Body.Close() })
	return r
}
func (h *harness) begin(t *testing.T) string {
	t.Helper()
	login := h.get(t, "/auth/login?returnTo=https://evil.invalid")
	if login.StatusCode != 302 {
		t.Fatalf("login %d", login.StatusCode)
	}
	authorization := h.get(t, login.Header.Get("Location"))
	if authorization.StatusCode != 302 {
		t.Fatalf("authorize %d", authorization.StatusCode)
	}
	return authorization.Header.Get("Location")
}
func (h *harness) login(t *testing.T) map[string]any {
	t.Helper()
	callback := h.get(t, h.begin(t))
	if callback.StatusCode != 302 || callback.Header.Get("Location") != "/" {
		b, _ := io.ReadAll(callback.Body)
		t.Fatalf("callback %d %s", callback.StatusCode, b)
	}
	foundCookie := false
	for _, cookie := range callback.Cookies() {
		if cookie.Name == h.service.cookieName("session") {
			foundCookie = true
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Secure != h.service.secure {
				t.Fatalf("unsafe session cookie: %+v", cookie)
			}
		}
	}
	if !foundCookie {
		t.Fatal("callback omitted session cookie")
	}
	var info map[string]any
	r := h.get(t, "/api/session")
	if e := json.NewDecoder(r.Body).Decode(&info); e != nil {
		t.Fatal(e)
	}
	return info
}
func (h *harness) post(t *testing.T, path, csrf, origin string) *http.Response {
	t.Helper()
	r, _ := http.NewRequest("POST", h.server.URL+path, nil)
	r.Header.Set("X-CSRF-Token", csrf)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	response, e := h.client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestLoginPKCECookieAndAdmin(t *testing.T) {
	h := setup(t, nil, nil)
	info := h.login(t)
	if info["authenticated"] != true || info["configured"] != true || info["csrfToken"] == "" {
		t.Fatalf("session %v", info)
	}
	user := info["user"].(map[string]any)
	if user["role"] != "admin" || user["subject"] != "subject-1" {
		t.Fatalf("user %v", user)
	}
	endpoint, _ := url.Parse(h.server.URL)
	cookies := h.client.Jar.Cookies(endpoint)
	if len(cookies) != 1 || cookies[0].Name != "marionette-session" || len(cookies[0].Value) != 43 {
		t.Fatalf("cookies %v", cookies)
	}
	h.service.mu.Lock()
	session := h.service.sessions[cookies[0].Value]
	h.service.mu.Unlock()
	if session == nil {
		t.Fatal("opaque session not stored server-side")
	}
	response := h.post(t, "/api/action", info["csrfToken"].(string), h.server.URL)
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	var current identity.User
	_ = json.NewDecoder(response.Body).Decode(&current)
	if current.Subject != "subject-1" {
		t.Fatal("identity context missing")
	}
}
func TestIdentityTokenRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claims  map[string]any
		corrupt bool
	}{
		{"nonce", map[string]any{"nonce": "wrong"}, false}, {"audience", map[string]any{"aud": "other-client"}, false}, {"issuer", map[string]any{"iss": "https://other.invalid"}, false}, {"expired", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, false}, {"signature", nil, true}, {"unsigned-email-verification-type", map[string]any{"email_verified": "true"}, false}, {"azp", map[string]any{"aud": []string{"client", "other"}, "azp": "other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t, nil, nil)
			h.provider.overrides = tc.claims
			h.provider.corrupt = tc.corrupt
			r := h.get(t, h.begin(t))
			if r.StatusCode != 401 {
				t.Fatalf("bad token accepted: %d", r.StatusCode)
			}
			r = h.get(t, "/api/devices")
			if r.StatusCode != 401 {
				t.Fatal("session created for invalid token")
			}
		})
	}
}

func TestStateBoundOneUseAndExpiry(t *testing.T) {
	for _, mode := range []string{"wrong-browser", "wrong-state", "expired", "replay"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t, nil, nil)
			callback := h.begin(t)
			u, _ := url.Parse(callback)
			state := u.Query().Get("state")
			switch mode {
			case "wrong-browser":
				h.client.Jar, _ = cookiejar.New(nil)
			case "wrong-state":
				q := u.Query()
				q.Set("state", "wrong")
				u.RawQuery = q.Encode()
				callback = u.String()
			case "expired":
				h.service.mu.Lock()
				tx := h.service.pending[state]
				tx.expires = time.Now().Add(-time.Second)
				h.service.pending[state] = tx
				h.service.mu.Unlock()
			case "replay":
				if r := h.get(t, callback); r.StatusCode != 302 {
					t.Fatal(r.StatusCode)
				}
			}
			if r := h.get(t, callback); r.StatusCode != 400 {
				t.Fatalf("transaction accepted: %d", r.StatusCode)
			}
			h.provider.mu.Lock()
			count := h.provider.exchanges
			h.provider.mu.Unlock()
			want := 0
			if mode == "replay" {
				want = 1
			}
			if count != want {
				t.Fatalf("invalid transaction reached token exchange: %d", count)
			}
		})
	}
}

func TestAdminAllowlistRequiresVerifiedEmail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
		subject  bool
		role     string
	}{{"verified", true, false, "admin"}, {"unverified", false, false, "user"}, {"explicit-subject", false, true, "admin"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t, func(c *Config) {
				if tc.subject {
					c.AdminSubjects = []string{"subject-1"}
					c.AdminEmails = nil
				}
			}, nil)
			h.provider.overrides = map[string]any{"email_verified": tc.verified, "role": "admin"}
			info := h.login(t)
			if info["user"].(map[string]any)["role"] != tc.role {
				t.Fatalf("wrong role %v", info)
			}
		})
	}
}

func TestCSRFLogoutAndRequestCancellation(t *testing.T) {
	started := make(chan context.Context, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream" {
			started <- r.Context()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(200)
	})
	h := setup(t, nil, next)
	info := h.login(t)
	csrf := info["csrfToken"].(string)
	appURL, _ := url.Parse(h.server.URL)
	oldCookies := h.client.Jar.Cookies(appURL)
	for _, token := range []string{"", "wrong"} {
		if r := h.post(t, "/api/action", token, h.server.URL); r.StatusCode != 403 {
			t.Fatal("invalid CSRF accepted")
		}
	}
	if r := h.post(t, "/auth/logout", csrf, "https://evil.invalid"); r.StatusCode != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	req, _ := http.NewRequest("GET", h.server.URL+"/api/stream", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", "https://evil.invalid")
	r, e := h.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("cross-origin websocket accepted")
	}
	finished := make(chan error, 1)
	go func() {
		r, e := h.client.Get(h.server.URL + "/api/stream")
		if r != nil {
			r.Body.Close()
		}
		finished <- e
	}()
	var active context.Context
	select {
	case active = <-started:
	case <-time.After(time.Second):
		t.Fatal("stream not started")
	}
	if r := h.post(t, "/auth/logout", csrf, h.server.URL); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	select {
	case <-active.Done():
	case <-time.After(time.Second):
		t.Fatal("logout did not cancel active request")
	}
	if e := <-finished; e != nil {
		t.Fatal(e)
	}
	if r := h.get(t, "/api/devices"); r.StatusCode != 401 {
		t.Fatal("logout did not revoke session")
	}
	h.client.Jar.SetCookies(appURL, oldCookies)
	if r := h.get(t, "/api/devices"); r.StatusCode != 401 {
		t.Fatal("revoked session cookie was reusable")
	}
}

func TestExpiryCancelsActiveRequest(t *testing.T) {
	started := make(chan context.Context, 1)
	h := setup(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- r.Context(); <-r.Context().Done() }))
	h.login(t)
	h.service.mu.Lock()
	for _, v := range h.service.sessions {
		v.cancel()
		v.expires = time.Now().Add(100 * time.Millisecond)
		v.ctx, v.cancel = context.WithDeadline(h.service.ctx, v.expires)
	}
	h.service.mu.Unlock()
	finished := make(chan error, 1)
	go func() {
		r, e := h.client.Get(h.server.URL + "/api/stream")
		if r != nil {
			r.Body.Close()
		}
		finished <- e
	}()
	select {
	case ctx := <-started:
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("expiry did not cancel request")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if e := <-finished; e != nil {
		t.Fatal(e)
	}
	if r := h.get(t, "/api/devices"); r.StatusCode != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestUnconfiguredAndInvalidConfiguration(t *testing.T) {
	s, e := New(context.Background(), Config{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	handler := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for path, want := range map[string]int{"/": 200, "/app.js": 200, "/api/session": 200, "/api/status": 503, "/api/devices": 503, "/auth/login": 503} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	for _, cfg := range []Config{{ClientID: "partial"}, {IssuerURL: "http://external.invalid", ClientID: "id", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback"}, {IssuerURL: "/relative", ClientID: "id", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback"}, {IssuerURL: "https://issuer.invalid", ClientID: "id", ClientSecret: "secret", RedirectURL: "https://app.invalid/auth/callback?next=https://evil.invalid"}, {SessionTTL: 25 * time.Hour}} {
		if service, e := New(context.Background(), cfg); e == nil {
			service.Close()
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
	}
}

func TestCookieFlagsAndBoundedState(t *testing.T) {
	h := setup(t, nil, nil)
	r := h.get(t, "/auth/login")
	cookies := r.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" {
		t.Fatalf("login cookie flags: %v", cookies)
	}
	h.service.mu.Lock()
	for i := len(h.service.pending); i < maxPending; i++ {
		h.service.pending[fmt.Sprint(i)] = transaction{expires: time.Now().Add(time.Minute)}
	}
	h.service.mu.Unlock()
	if r := h.get(t, "/auth/login"); r.StatusCode != 503 {
		t.Fatal("unbounded pending logins")
	}
	w := httptest.NewRecorder()
	h.service.secure = true
	h.service.cookie(w, "session", "opaque", time.Now().Add(time.Hour))
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.Name != "__Host-marionette-session" || c.Domain != "" {
		t.Fatalf("secure cookie flags: %+v", c)
	}
}

func TestSeparateFrontendCanReadUnconfiguredLoginState(t *testing.T) {
	const front = "http://127.0.0.1:3000"
	s, err := New(context.Background(), Config{FrontendOrigins: []string{front}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := s.Wrap(http.NotFoundHandler())
	for _, tc := range []struct {
		origin, path string
		status       int
	}{
		{front, "/api/session", 200},
		{front, "/api/devices", 503},
		{"https://unlisted.invalid", "/api/session", 403},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.origin, tc.path, w.Code, w.Body)
		}
		if tc.status == 200 && !strings.Contains(w.Body.String(), `"configured":false`) {
			t.Fatalf("missing login configuration state: %s", w.Body)
		}
	}
}

func TestSeparateFrontendCORSAndCookie(t *testing.T) {
	const front = "https://app.example.com"
	h := setup(t, func(c *Config) { c.FrontendOrigins = []string{front, "https://app.example.com/"} }, nil)

	preflight, _ := http.NewRequest("OPTIONS", h.server.URL+"/api/devices/x/lease", nil)
	preflight.Header.Set("Origin", front)
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	r, e := h.client.Do(preflight)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 204 || r.Header.Get("Access-Control-Allow-Origin") != front || r.Header.Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "X-CSRF-Token") {
		t.Fatalf("preflight %d %v", r.StatusCode, r.Header)
	}

	evil, _ := http.NewRequest("OPTIONS", h.server.URL+"/api/devices", nil)
	evil.Header.Set("Origin", "https://evil.invalid")
	evil.Header.Set("Access-Control-Request-Method", "GET")
	r, e = h.client.Do(evil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin received CORS grant")
	}

	session, _ := http.NewRequest("GET", h.server.URL+"/api/session", nil)
	session.Header.Set("Origin", front)
	session.Header.Set("Sec-Fetch-Site", "cross-site")
	r, e = h.client.Do(session)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("Access-Control-Allow-Origin") != front {
		t.Fatalf("frontend session read %d", r.StatusCode)
	}
}

func TestFrontendLoginReturnsToFrontend(t *testing.T) {
	h := setup(t, func(c *Config) { c.FrontendOrigins = []string{"http://localhost:5173"} }, nil)
	callback := h.get(t, h.begin(t))
	if callback.StatusCode != 302 || callback.Header.Get("Location") != "http://localhost:5173/" {
		t.Fatalf("callback %d %q", callback.StatusCode, callback.Header.Get("Location"))
	}
}

func TestInvalidFrontendOrigin(t *testing.T) {
	p := provider(t)
	for _, origin := range []string{"ftp://x.example", "https://x.example/path", "http://x.example"} {
		_, e := New(context.Background(), Config{IssuerURL: p.server.URL, ClientID: "c", ClientSecret: "s", RedirectURL: "http://127.0.0.1:1/auth/callback", FrontendOrigins: []string{origin}})
		if origin != "http://x.example" && e == nil {
			t.Fatalf("accepted %q", origin)
		}
		if origin == "http://x.example" && e == nil {
			t.Fatalf("accepted non-loopback http origin")
		}
	}
}
