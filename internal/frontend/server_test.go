package frontend

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneFrontend(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("app-shell"), 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := New(dir, "https://api.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/admin", "/devices", "/login"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != 200 || method == "GET" && w.Body.String() != "app-shell" {
				t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
			}
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/config.js", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"https://api.example.com"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("runtime config: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/devices", "/missing.js", "/admin/child"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("frontend handled backend/missing route %s: %d", path, w.Code)
		}
	}
}
