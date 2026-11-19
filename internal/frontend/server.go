// Package frontend serves only the web console and its runtime API address.
package frontend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"marionette/internal/service"
)

func New(dir, apiURL string) (http.Handler, error) {
	if apiURL != "" {
		var err error
		apiURL, err = service.Origin(strings.TrimRight(apiURL, "/"))
		if err != nil {
			return nil, fmt.Errorf("--api-url / MARIONETTE_API_URL %w", err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "index.html")); dir == "" || err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("--web-dir / MARIONETTE_WEB_DIR requires a directory with index.html")
	}
	encoded, _ := json.Marshal(apiURL)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(append(append([]byte("globalThis.MARIONETTE_API_URL = "), encoded...), ';', '\n'))
	})
	for _, path := range []string{"/{$}", "/admin", "/devices", "/login"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
		})
	}
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Always revalidate so a new deployment never mixes cached modules with new HTML.
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		mux.ServeHTTP(w, r)
	}), nil
}
