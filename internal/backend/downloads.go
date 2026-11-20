package backend

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MountAgentDownloads registers the public bootstrap assets outside browser auth.
// The package directory is never exposed as a file server.
func MountAgentDownloads(mux *http.ServeMux, packagesDir, installScript string) {
	mux.HandleFunc("/downloads/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.EscapedPath(), "/downloads/")
		switch name {
		case "marionette-agent-linux-amd64.tar.gz", "marionette-agent-linux-arm64.tar.gz":
			serveAgentAsset(w, r, "application/gzip", filepath.Join(packagesDir, name))
		case "marionette-agent-linux-SHA256SUMS":
			serveAgentAsset(w, r, "text/plain; charset=utf-8", filepath.Join(packagesDir, name))
		default:
			http.NotFound(w, r)
		}
	})
	const defaultScript = "scripts/setup-ubuntu-agent.sh"
	scriptPaths := []string{installScript}
	if installScript == "" {
		sibling := filepath.Join(filepath.Dir(filepath.Clean(packagesDir)), defaultScript)
		scriptPaths = []string{sibling}
		if sibling != defaultScript {
			scriptPaths = append(scriptPaths, defaultScript)
		}
	}
	const installPath = "/install/ubuntu.sh"
	mux.HandleFunc(installPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != installPath {
			http.NotFound(w, r)
			return
		}
		serveAgentAsset(w, r, "text/x-shellscript; charset=utf-8", scriptPaths...)
	})
}

func serveAgentAsset(w http.ResponseWriter, r *http.Request, contentType string, paths ...string) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	for _, path := range paths {
		file, err := openAgentAsset(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			slog.Error("open agent download", "path", path, "error", err)
			break
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			break
		}
		w.Header().Set("Content-Type", contentType)
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
		return
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "agent installation files are unavailable; build and deploy both Linux agent packages and marionette-agent-linux-SHA256SUMS to the backend agent-packages directory", http.StatusServiceUnavailable)
}

func openAgentAsset(path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	return root.Open(name)
}
