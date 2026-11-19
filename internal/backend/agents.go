package backend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"marionette/internal/agent"
	"marionette/internal/identity"
)

type agentRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"tokenHash"`
	CreatedAt time.Time `json:"createdAt"`
}

// EnableAgentStore persists registration hashes; raw connection tokens are shown once.
func (s *Server) EnableAgentStore(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var records []agentRecord
	if err == nil {
		if len(data) > 2<<20 || json.Unmarshal(data, &records) != nil || len(records) > 256 {
			return errors.New("invalid agent registration store")
		}
		seen := make(map[string]bool)
		for _, record := range records {
			decoded, err := hex.DecodeString(record.Hash)
			if !agent.ValidID(record.ID) || len(record.Name) > 100 || err != nil || len(decoded) != sha256.Size || seen[record.ID] {
				return errors.New("invalid agent registration record")
			}
			if existing := s.tokens[record.ID]; existing != "" && existing != record.Hash {
				return fmt.Errorf("agent %q has different credentials in the environment and agent store", record.ID)
			}
			seen[record.ID] = true
			s.registry[record.ID], s.tokens[record.ID] = record, record.Hash
		}
	}
	s.agentFile = path
	return nil
}

func (s *Server) saveAgentsLocked() error {
	records := make([]agentRecord, 0, len(s.registry))
	for _, record := range s.registry {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.agentFile), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.agentFile), ".agents-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), s.agentFile)
	}
	return err
}

func (s *Server) registerAgent(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		respond(w, 403, map[string]string{"error": "administrator required"})
		return
	}
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if decode(w, r, &body) != nil || !agent.ValidID(body.ID) || len(strings.TrimSpace(body.Name)) == 0 || len(body.Name) > 100 {
		respond(w, 400, map[string]string{"error": "ID must contain 1–64 letters, digits, underscores or hyphens; a name up to 100 bytes is required"})
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		respond(w, 500, map[string]string{"error": "could not generate agent token"})
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.agentFile == "" {
		respond(w, 503, map[string]string{"error": "agent registration store is unavailable"})
		return
	}
	if s.registry[body.ID].ID != "" {
		respond(w, 409, map[string]string{"error": "this agent ID is already registered"})
		return
	}
	if len(s.registry) >= 256 {
		respond(w, 409, map[string]string{"error": "agent registration limit reached"})
		return
	}
	record := agentRecord{ID: body.ID, Name: strings.TrimSpace(body.Name), Hash: hex.EncodeToString(hash[:]), CreatedAt: time.Now()}
	s.registry[body.ID], s.tokens[body.ID] = record, record.Hash
	if err := s.saveAgentsLocked(); err != nil {
		delete(s.registry, body.ID)
		delete(s.tokens, body.ID)
		respond(w, 500, map[string]string{"error": "could not persist agent registration"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 201, map[string]any{"id": record.ID, "name": record.Name, "token": token})
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		respond(w, 403, map[string]string{"error": "administrator required"})
		return
	}
	details := make(map[string]map[string]any)
	for _, result := range s.collect(r, "/api/agent") {
		var detail map[string]any
		if result.err == nil && json.Unmarshal(result.body, &detail) == nil {
			details[result.id] = detail
		}
	}
	s.mu.Lock()
	records := make([]map[string]any, 0, len(s.registry))
	for id, record := range s.registry {
		p := s.peers[id]
		connected := p != nil && !p.session.IsClosed()
		if connected {
			s.lastSeen[id] = time.Now()
		}
		records = append(records, map[string]any{"id": id, "name": record.Name, "connected": connected, "lastSeen": s.lastSeen[id], "createdAt": record.CreatedAt, "machine": details[id]})
	}
	s.mu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i]["id"].(string) < records[j]["id"].(string) })
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"agents": records})
}

func (s *Server) configureAgent(w http.ResponseWriter, r *http.Request) {
	if !identity.IsAdmin(r.Context()) {
		respond(w, 403, map[string]string{"error": "administrator required"})
		return
	}
	s.mu.Lock()
	p := s.peers[r.PathValue("id")]
	s.mu.Unlock()
	if p == nil || p.session.IsClosed() {
		respond(w, 503, map[string]string{"error": "agent is offline"})
		return
	}
	user, _ := identity.FromContext(r.Context())
	proxy := &httputil.ReverseProxy{Transport: p.transport, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "http", Host: "localhost"})
		pr.Out.URL.Path, pr.Out.URL.RawPath, pr.Out.URL.RawQuery = "/api/admin/agent", "", ""
		forwardIdentity(pr.Out, user)
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		respond(w, 502, map[string]string{"error": "agent configuration request failed"})
	}}
	proxy.ServeHTTP(w, r)
}
