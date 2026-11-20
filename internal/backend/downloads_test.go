package backend_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"marionette/internal/backend"
)

func TestAgentInstallationDownloads(t *testing.T) {
	root := t.TempDir()
	packages := filepath.Join(root, "packages")
	if err := os.Mkdir(packages, 0755); err != nil {
		t.Fatal(err)
	}
	assets := map[string]string{
		"marionette-agent-linux-amd64.tar.gz": "agent archive",
		"marionette-agent-linux-SHA256SUMS":   "agent checksums",
	}
	for name, body := range assets {
		if err := os.WriteFile(filepath.Join(packages, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "setup-ubuntu-agent.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	backend.MountAgentDownloads(mux, packages, script)
	for _, tc := range []struct {
		name, method, path string
		status             int
		body               string
	}{
		{"installer", "GET", "/install/ubuntu.sh", 200, "#!/usr/bin/env bash"},
		{"checksums", "GET", "/downloads/marionette-agent-linux-SHA256SUMS", 200, "agent checksums"},
		{"package", "GET", "/downloads/marionette-agent-linux-amd64.tar.gz", 200, "agent archive"},
		{"head", "HEAD", "/downloads/marionette-agent-linux-amd64.tar.gz", 200, ""},
		{"missing release artifact", "GET", "/downloads/marionette-agent-linux-arm64.tar.gz", 503, "build and deploy"},
		{"private file", "GET", "/downloads/.env", 404, "404 page not found"},
		{"unsupported method", "POST", "/downloads/marionette-agent-linux-amd64.tar.gz", 405, "method not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.body) {
				t.Fatalf("status=%d body=%q; want status=%d containing %q", response.Code, response.Body.String(), tc.status, tc.body)
			}
			if tc.status == 503 && response.Header().Get("Retry-After") == "" {
				t.Fatal("missing release artifact must indicate temporary unavailability")
			}
		})
	}
}
