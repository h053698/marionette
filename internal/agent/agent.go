// Package agent connects the device computer to the relay without opening a
// listening port on that computer. OIDC and browser credentials stay at the relay.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"marionette/internal/identity"
	"marionette/internal/tunnel"
)

const IdentityHeader = "X-Marionette-Identity"

var agentID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidID(id string) bool { return agentID.MatchString(id) }

type Config struct {
	BackendURL string
	ID         string
	Token      string
}

func (cfg Config) endpoint() (string, error) {
	u, err := url.Parse(cfg.BackendURL)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("backend URL must be an HTTP(S) origin")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
	switch {
	case u.Scheme == "https":
		u.Scheme = "wss"
	case u.Scheme == "http" && loopback:
		u.Scheme = "ws"
	default:
		return "", errors.New("backend URL requires HTTPS except on loopback")
	}
	if !ValidID(cfg.ID) || len(cfg.Token) < 32 || strings.ContainsAny(cfg.Token, " \t\r\n") {
		return "", errors.New("agent ID and a token of at least 32 bytes are required")
	}
	u.Path = "/internal/agents/" + cfg.ID + "/connect"
	return u.String(), nil
}

// Run reconnects with bounded backoff. serve creates a fresh runtime for each
// connection, so losing the relay revokes leases and stops device processes.
func Run(ctx context.Context, cfg Config, serve func(context.Context, net.Listener) error) error {
	endpoint, err := cfg.endpoint()
	if err != nil {
		return err
	}
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err = connect(ctx, endpoint, cfg.Token, serve)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("agent disconnected; reconnecting", "agent", cfg.ID, "error", err, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		} else {
			delay = min(delay*2, 30*time.Second)
		}
	}
	return nil
}

func connect(ctx context.Context, endpoint, token string, serve func(context.Context, net.Listener) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent tunnel worker panicked", "panicType", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
			err = errors.New("agent tunnel worker failed")
		}
	}()
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{tunnel.Protocol}}
	ws, response, err := dialer.DialContext(ctx, endpoint, http.Header{"Authorization": {"Bearer " + token}})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connect relay: %w", err)
	}
	defer ws.Close()
	if ws.Subprotocol() != tunnel.Protocol {
		return errors.New("relay did not negotiate agent protocol")
	}
	session, err := yamux.Server(tunnel.New(ws), tunnel.Config())
	if err != nil {
		return err
	}
	defer session.Close()
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sessionCtx, func() { session.Close() })
	defer stop()
	go func() {
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("agent tunnel watcher panicked", "panicType", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
			}
		}()
		select {
		case <-session.CloseChan():
		case <-sessionCtx.Done():
		}
	}()
	slog.Info("agent connected to relay")
	return serve(sessionCtx, session)
}

// WithIdentity accepts identity only on the authenticated, private tunnel. The
// agent has no TCP HTTP listener; the backend overwrites this header on requests.
func WithIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := base64.RawURLEncoding.DecodeString(r.Header.Get(IdentityHeader))
		var user identity.User
		if err != nil || json.Unmarshal(data, &user) != nil || user.Subject == "" || (user.Role != "user" && user.Role != "admin") {
			http.Error(w, "relay identity required", http.StatusUnauthorized)
			return
		}
		r.Header.Del(IdentityHeader)
		next.ServeHTTP(w, r.WithContext(identity.WithUser(r.Context(), user)))
	})
}
