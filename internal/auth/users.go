package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"marionette/internal/identity"
)

const (
	maxUsers          = 4096
	userTouchInterval = time.Minute
	userFlushInterval = 5 * time.Second
	maxSubjectBytes   = 512
	maxEmailBytes     = 320
	maxNameBytes      = 200
)

// knownUser is the only data the registry keeps about a member: no tokens,
// cookies, CSRF values or roles. Role is recomputed from the admin settings.
type knownUser struct {
	Subject       string    `json:"subject"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	EmailVerified bool      `json:"emailVerified"`
	LastLoginAt   time.Time `json:"lastLoginAt"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
}

// Member is an administrator's view of one known website user.
type Member struct {
	Subject, Email, Name, Role string
	LastLoginAt, LastSeenAt    time.Time
	Online                     bool
	ActiveSessions, TokenCount int
}

// roleFor applies the same administrator rules at login and in listings.
func (s *Service) roleFor(subject, email string, verified bool) string {
	if s.subjects[subject] || (verified && s.admins[strings.ToLower(strings.TrimSpace(email))]) {
		return "admin"
	}
	return "user"
}

func clip(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Service) loadUsers() error {
	if s.userFile == "" {
		return nil
	}
	file, err := os.Open(s.userFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open user registry: %w", err)
	}
	defer file.Close()
	var records []knownUser
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	if err := decoder.Decode(&records); err != nil {
		return fmt.Errorf("read user registry: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || len(records) > maxUsers {
		return errors.New("invalid user registry")
	}
	for _, record := range records {
		if record.Subject == "" || len(record.Subject) > maxSubjectBytes || len(record.Email) > maxEmailBytes || len(record.Name) > maxNameBytes || s.users[record.Subject] != nil {
			return errors.New("invalid user registry record")
		}
		record := record
		s.users[record.Subject] = &record
	}
	return nil
}

// flushUsers writes the registry atomically with owner-only permissions. The
// save lock is taken first so a stale snapshot never overwrites a newer one.
func (s *Service) flushUsers() error {
	if s.userFile == "" {
		return nil
	}
	s.userSaveMu.Lock()
	defer s.userSaveMu.Unlock()
	s.usersMu.Lock()
	records := make([]knownUser, 0, len(s.users))
	for _, user := range s.users {
		records = append(records, *user)
	}
	s.usersDirty = false
	s.usersFlushed = time.Now()
	s.usersMu.Unlock()
	sort.Slice(records, func(i, j int) bool { return records[i].Subject < records[j].Subject })
	err := writeAtomic(s.userFile, ".users-*", records)
	if err != nil {
		s.usersMu.Lock()
		s.usersDirty = true
		s.usersMu.Unlock()
	}
	return err
}

func writeAtomic(path, pattern string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(v)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	return err
}

// flushUsersIfDue batches the once-a-minute last-seen updates into one write.
func (s *Service) flushUsersIfDue() {
	s.usersMu.Lock()
	due := s.usersDirty && time.Since(s.usersFlushed) >= userFlushInterval
	s.usersMu.Unlock()
	if due {
		if err := s.flushUsers(); err != nil {
			slog.Warn("could not save user registry", "error", err)
		}
	}
}

// upsertUserLocked creates or returns a registry entry, evicting the entry seen
// longest ago when the bounded registry is full. Called with s.usersMu held.
func (s *Service) upsertUserLocked(subject string) *knownUser {
	if u := s.users[subject]; u != nil {
		return u
	}
	if len(s.users) >= maxUsers {
		var oldest *knownUser
		for _, u := range s.users {
			if oldest == nil || u.LastSeenAt.Before(oldest.LastSeenAt) {
				oldest = u
			}
		}
		delete(s.users, oldest.Subject)
	}
	u := &knownUser{Subject: subject}
	s.users[subject] = u
	return u
}

func (s *Service) fillUserLocked(u *knownUser, user identity.User, verified bool) {
	u.Email, u.Name, u.EmailVerified = clip(user.Email, maxEmailBytes), clip(user.Name, maxNameBytes), verified
}

func (s *Service) recordLogin(user identity.User, verified bool, now time.Time) {
	if user.Subject == "" || len(user.Subject) > maxSubjectBytes {
		return
	}
	s.usersMu.Lock()
	u := s.upsertUserLocked(user.Subject)
	s.fillUserLocked(u, user, verified)
	u.LastLoginAt, u.LastSeenAt = now, now
	s.usersDirty = true
	s.usersMu.Unlock()
	if err := s.flushUsers(); err != nil {
		slog.Warn("could not save user registry", "error", err)
	}
}

// touchUser refreshes last-seen at most once a minute per user. Only browser
// sessions may add a missing entry; API tokens just refresh an existing one.
func (s *Service) touchUser(v *session, bearer bool) {
	if v.user.Subject == "" || len(v.user.Subject) > maxSubjectBytes {
		return
	}
	now := time.Now()
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	u := s.users[v.user.Subject]
	if u == nil {
		if bearer {
			return
		}
		u = s.upsertUserLocked(v.user.Subject)
		s.fillUserLocked(u, v.user, v.verified)
		u.LastLoginAt = v.started
	} else if now.Sub(u.LastSeenAt) < userTouchInterval {
		return
	}
	u.LastSeenAt = now
	s.usersDirty = true
}

// Members lists every known user with live session and token counts. Role is the
// current administrator decision, never a stored value. Callers must authorize.
func (s *Service) Members() []Member {
	type live struct {
		sessions, tokens int
		user             identity.User
		verified         bool
		started          time.Time
	}
	now := time.Now()
	lives := make(map[string]*live)
	entry := func(subject string) *live {
		if lives[subject] == nil {
			lives[subject] = &live{}
		}
		return lives[subject]
	}
	s.mu.Lock()
	s.pruneLocked()
	for _, v := range s.sessions {
		if now.Before(v.expires) && v.ctx.Err() == nil {
			l := entry(v.user.Subject)
			l.sessions++
			l.user, l.verified, l.started = v.user, v.verified, v.started
		}
	}
	for _, t := range s.tokens {
		if now.Before(t.ExpiresAt) && t.ctx.Err() == nil {
			entry(t.User.Subject).tokens++
		}
	}
	s.mu.Unlock()
	s.usersMu.Lock()
	records := make(map[string]knownUser, len(s.users))
	for subject, u := range s.users {
		records[subject] = *u
	}
	s.usersMu.Unlock()
	for subject, l := range lives {
		if _, ok := records[subject]; !ok && l.sessions > 0 {
			records[subject] = knownUser{Subject: subject, Email: clip(l.user.Email, maxEmailBytes), Name: clip(l.user.Name, maxNameBytes), EmailVerified: l.verified, LastLoginAt: l.started, LastSeenAt: now}
		}
	}
	members := make([]Member, 0, len(records))
	for subject, u := range records {
		m := Member{Subject: subject, Email: u.Email, Name: u.Name, Role: s.roleFor(subject, u.Email, u.EmailVerified), LastLoginAt: u.LastLoginAt, LastSeenAt: u.LastSeenAt}
		if l := lives[subject]; l != nil {
			m.ActiveSessions, m.TokenCount, m.Online = l.sessions, l.tokens, l.sessions > 0
		}
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		a, b := members[i], members[j]
		if a.Online != b.Online {
			return a.Online
		}
		if !a.LastSeenAt.Equal(b.LastSeenAt) {
			return a.LastSeenAt.After(b.LastSeenAt)
		}
		if ae, be := strings.ToLower(a.Email), strings.ToLower(b.Email); ae != be {
			return ae < be
		}
		return a.Subject < b.Subject
	})
	return members
}
