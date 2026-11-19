package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"marionette/internal/identity"
)

const maxTokensPerUser = 20

type tokenInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type storedToken struct {
	tokenInfo
	Hash string        `json:"hash"`
	User identity.User `json:"user"`
}

type apiToken struct {
	storedToken
	*session
}

func tokenHash(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func (s *Service) liveToken(record storedToken) *apiToken {
	// A PAT can control leased devices but never grants admin previews or minting.
	record.User.Role = "user"
	ctx, cancel := context.WithDeadline(s.ctx, record.ExpiresAt)
	return &apiToken{record, &session{user: record.User, expires: record.ExpiresAt, ctx: ctx, cancel: cancel}}
}

func (s *Service) lookupToken(r *http.Request) *session {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) != 47 || !strings.HasPrefix(parts[1], "mnt_") {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	token := s.tokens[tokenHash(parts[1])]
	if token == nil || token.ctx.Err() != nil {
		return nil
	}
	return token.session
}

func (s *Service) loadTokens() error {
	if s.tokenFile == "" {
		return nil
	}
	file, err := os.Open(s.tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open API token store: %w", err)
	}
	defer file.Close()
	var records []storedToken
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	if err := decoder.Decode(&records); err != nil {
		return fmt.Errorf("read API token store: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(records) > maxSessions {
		return errors.New("invalid API token store")
	}
	ids := make(map[string]bool)
	for _, record := range records {
		hash, err := hex.DecodeString(record.Hash)
		if err != nil || len(hash) != 32 || record.ID == "" || record.User.Subject == "" || record.ExpiresAt.IsZero() || ids[record.ID] || s.tokens[record.Hash] != nil {
			return errors.New("invalid API token record")
		}
		ids[record.ID] = true
		if time.Now().Before(record.ExpiresAt) {
			s.tokens[record.Hash] = s.liveToken(record)
		}
	}
	return nil
}

// Called with s.mu held. Atomic replacement leaves the previous file intact on error.
func (s *Service) saveTokensLocked() error {
	if s.tokenFile == "" {
		return nil
	}
	records := make([]storedToken, 0, len(s.tokens))
	for _, token := range s.tokens {
		records = append(records, token.storedToken)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	dir := filepath.Dir(s.tokenFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(records)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), s.tokenFile)
	}
	return err
}

func (s *Service) manageTokens(w http.ResponseWriter, r *http.Request, user identity.User) {
	w.Header().Set("Cache-Control", "no-store")
	create := r.URL.Path == "/api/tokens" && r.Method == http.MethodPost
	var input struct {
		Name string `json:"name"`
		Days int    `json:"expiresInDays"`
	}
	if create {
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			failure(w, 500, "could not set token request deadline")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&input)
		if err == nil {
			var extra any
			if trailing := decoder.Decode(&extra); trailing != io.EOF {
				err = trailing
				if err == nil {
					err = errors.New("extra token JSON")
				}
			}
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			failure(w, http.StatusRequestTimeout, "token request body timed out")
			return
		}
		if err != nil {
			failure(w, 400, "invalid token JSON")
			return
		}
		_ = controller.SetReadDeadline(time.Time{})
		input.Name = strings.TrimSpace(input.Name)
		if input.Days == 0 {
			input.Days = 30
		}
		if input.Name == "" || len(input.Name) > 100 || input.Days < 1 || input.Days > 90 {
			failure(w, 400, "name must contain 1–100 bytes; expiresInDays must be between 1 and 90")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if r.URL.Path == "/api/tokens" && r.Method == http.MethodGet {
		items := make([]tokenInfo, 0)
		for _, token := range s.tokens {
			if token.User.Subject == user.Subject {
				items = append(items, token.tokenInfo)
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		respond(w, 200, map[string]any{"tokens": items})
		return
	}
	if create {
		cookie, err := r.Cookie(s.cookieName("session"))
		if err != nil {
			failure(w, 401, "sign-in required")
			return
		}
		current := s.sessions[cookie.Value]
		if current == nil || current.user.Subject != user.Subject || current.ctx.Err() != nil || !time.Now().Before(current.expires) {
			failure(w, 401, "sign-in required")
			return
		}
		if !equal(r.Header.Get("X-CSRF-Token"), current.csrf) {
			failure(w, 403, "invalid CSRF token")
			return
		}
		count := 0
		for _, token := range s.tokens {
			if token.User.Subject == user.Subject {
				count++
			}
		}
		if count >= maxTokensPerUser || len(s.tokens) >= maxSessions || s.ctx.Err() != nil {
			failure(w, 409, "API token limit reached; revoke an existing token first")
			return
		}
		now := time.Now()
		raw := "mnt_" + secret()
		record := storedToken{tokenInfo: tokenInfo{ID: secret(), Name: input.Name, CreatedAt: now, ExpiresAt: now.Add(time.Duration(input.Days) * 24 * time.Hour)}, Hash: tokenHash(raw), User: user}
		token := s.liveToken(record)
		s.tokens[record.Hash] = token
		if err := s.saveTokensLocked(); err != nil {
			delete(s.tokens, record.Hash)
			token.cancel()
			failure(w, 500, "could not save API token")
			return
		}
		respond(w, 201, struct {
			tokenInfo
			Token string `json:"token"`
		}{token.tokenInfo, raw})
		return
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/tokens/") {
		id := strings.TrimPrefix(r.URL.Path, "/api/tokens/")
		for hash, token := range s.tokens {
			if token.ID != id || token.User.Subject != user.Subject {
				continue
			}
			delete(s.tokens, hash)
			if err := s.saveTokensLocked(); err != nil {
				s.tokens[hash] = token
				failure(w, 500, "could not revoke API token")
				return
			}
			token.cancel()
			respond(w, 200, map[string]bool{"ok": true})
			return
		}
		failure(w, 404, "unknown API token")
		return
	}
	w.Header().Set("Allow", "GET, POST, DELETE")
	failure(w, 405, "unsupported token operation")
}
