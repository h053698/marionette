package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"marionette/internal/identity"
)

func tokenRequest(t *testing.T, h *harness, method, path, body, csrf, bearer string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-CSRF-Token", csrf)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := h.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func issueToken(t *testing.T, h *harness, csrf string) (raw, id string) {
	t.Helper()
	r := tokenRequest(t, h, "POST", "/api/tokens", `{"name":"MCP test","expiresInDays":1}`, csrf, "")
	if r.StatusCode != 201 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("issue: %d %s", r.StatusCode, b)
	}
	var result struct{ Token, ID string }
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Token) != 47 || !strings.HasPrefix(result.Token, "mnt_") || result.ID == "" {
		t.Fatal("invalid token response")
	}
	return result.Token, result.ID
}

func TestAPITokenAuthenticationAndRevocation(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	h := setup(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/wait" {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		user, _ := identity.FromContext(r.Context())
		respond(w, 200, user)
	}))
	csrf := h.login(t)["csrfToken"].(string)
	if r := tokenRequest(t, h, "POST", "/api/tokens", `{"name":"no csrf"}`, "", ""); r.StatusCode != 403 {
		t.Fatal("mint bypassed CSRF")
	}
	raw, id := issueToken(t, h, csrf)
	for _, method := range []string{"GET", "POST"} {
		r := tokenRequest(t, h, method, "/api/action", `{}`, "", raw)
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		var user identity.User
		_ = json.NewDecoder(r.Body).Decode(&user)
		if user.Subject != "subject-1" || user.Role != "user" {
			t.Fatalf("PAT identity: %+v", user)
		}
	}
	for _, path := range []string{"/api/tokens", "/api/tokens/" + id, "/auth/logout"} {
		if r := tokenRequest(t, h, "DELETE", path, "", csrf, raw); r.StatusCode != 403 {
			t.Fatalf("PAT management allowed %s %d", path, r.StatusCode)
		}
	}
	if r := tokenRequest(t, h, "POST", "/api/action", "{}", csrf, "invalid"); r.StatusCode != 401 {
		t.Fatal("invalid bearer fell back to cookie")
	}
	listing := h.get(t, "/api/tokens")
	listed, _ := io.ReadAll(listing.Body)
	if strings.Contains(string(listed), raw) || strings.Contains(string(listed), "hash") {
		t.Fatal("list leaked token credential")
	}
	req, _ := http.NewRequest("GET", h.server.URL+"/api/wait", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, err := h.client.Do(req)
		if err == nil {
			r.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	if r := tokenRequest(t, h, "DELETE", "/api/tokens/"+id, "", csrf, ""); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("revoke did not cancel request")
	}
	<-done
	if r := tokenRequest(t, h, "GET", "/api/action", "", "", raw); r.StatusCode != 401 {
		t.Fatal("revoked token accepted")
	}
}

func TestTokenPersistenceIsolationAndExpiry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "private", "tokens.json")
	h := setup(t, func(cfg *Config) { cfg.TokenFile = file }, nil)
	csrf := h.login(t)["csrfToken"].(string)
	raw, id := issueToken(t, h, csrf)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), raw) || !strings.Contains(string(data), tokenHash(raw)) {
		t.Fatal("store must contain hash only")
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", st.Mode())
	}
	restored := &Service{ctx: context.Background(), tokenFile: file, tokens: make(map[string]*apiToken)}
	if err := restored.loadTokens(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, token := range restored.tokens {
			token.cancel()
		}
	})
	r := httptest.NewRequest("GET", "/api/devices", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	if session := restored.lookupToken(r); session == nil || session.user.Role != "user" {
		t.Fatal("restart lost token")
	}
	h.provider.mu.Lock()
	h.provider.overrides["sub"] = "subject-2"
	h.provider.mu.Unlock()
	otherCSRF := h.login(t)["csrfToken"].(string)
	response := tokenRequest(t, h, "DELETE", "/api/tokens/"+id, "", otherCSRF, "")
	if response.StatusCode != 404 {
		t.Fatal("other user revoked token")
	}
	response = h.get(t, "/api/tokens")
	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), id) {
		t.Fatal("other user listed token")
	}
	// Expiry must be enforced on each request even before the cleaner tick.
	h.service.mu.Lock()
	h.service.tokens[tokenHash(raw)].ExpiresAt = time.Now().Add(-time.Second)
	h.service.mu.Unlock()
	if response = tokenRequest(t, h, "GET", "/api/action", "", "", raw); response.StatusCode != 401 {
		t.Fatal("expired token accepted")
	}
}

func TestTokenValidationAndStorageFailure(t *testing.T) {
	h := setup(t, nil, nil)
	csrf := h.login(t)["csrfToken"].(string)
	for _, body := range []string{`{}`, `{"name":"a","expiresInDays":91}`, `{"name":"a","expiresInDays":-1}`, `{"name":"a","extra":1}`, `{"name":"a"} {}`} {
		if r := tokenRequest(t, h, "POST", "/api/tokens", body, csrf, ""); r.StatusCode != 400 {
			t.Fatalf("accepted %s", body)
		}
	}
	// A file as parent makes atomic writes fail without granting a usable token.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h.service.tokenFile = filepath.Join(file, "tokens.json")
	if r := tokenRequest(t, h, "POST", "/api/tokens", `{"name":"a"}`, csrf, ""); r.StatusCode != 500 {
		t.Fatal(r.StatusCode)
	}
	h.service.mu.Lock()
	count := len(h.service.tokens)
	h.service.mu.Unlock()
	if count != 0 {
		t.Fatal("failed persistence kept live token")
	}
}
