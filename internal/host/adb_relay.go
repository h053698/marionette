package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// OpenService selects a USB transport before requesting a device service. The
// public relay never exposes the host's smart socket or host:* commands.
type serviceProvider interface {
	OpenService(context.Context, adbTarget, string) (net.Conn, error)
}

func (a ExecADB) OpenService(ctx context.Context, target adbTarget, service string) (net.Conn, error) {
	if !deviceService(service) {
		return nil, errors.New("unsupported device ADB service")
	}
	network, address := "tcp", "127.0.0.1:5037"
	if port := os.Getenv("ANDROID_ADB_SERVER_PORT"); port != "" {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	if socket := os.Getenv("ADB_SERVER_SOCKET"); socket != "" {
		switch {
		case strings.HasPrefix(socket, "tcp:"):
			address = strings.TrimPrefix(socket, "tcp:")
		case strings.HasPrefix(socket, "localfilesystem:"):
			network, address = "unix", strings.TrimPrefix(socket, "localfilesystem:")
		default:
			return nil, errors.New("unsupported ADB_SERVER_SOCKET")
		}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	transport := "host:transport:" + target.serial
	if target.transportID != "" {
		transport = "host:transport-id:" + target.transportID
	}
	for _, request := range []string{transport, service} {
		if len(request) > 65535 {
			conn.Close()
			return nil, errors.New("ADB service too long")
		}
		if _, err = io.WriteString(conn, fmt.Sprintf("%04x%s", len(request), request)); err != nil {
			conn.Close()
			return nil, err
		}
		var status [4]byte
		if _, err = io.ReadFull(conn, status[:]); err != nil {
			conn.Close()
			return nil, err
		}
		if string(status[:]) != "OKAY" {
			// Consume a bounded failure body without echoing shell commands or paths.
			var length [4]byte
			_, _ = io.ReadFull(conn, length[:])
			n, _ := strconv.ParseUint(string(length[:]), 16, 16)
			_, _ = io.CopyN(io.Discard, conn, int64(n))
			conn.Close()
			return nil, errors.New("device rejected ADB service")
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func deviceService(service string) bool {
	if service == "" || len(service) > 65535 || strings.ContainsRune(service, 0) {
		return false
	}
	// Reverse forwarding terminates on the agent's ADB server, not the user's
	// PC. Do not expose agent-local ports or sockets through the assigned device.
	// Host queries, transport switching and server shutdown are also absent.
	for _, prefix := range []string{"shell:", "shell,v2,", "exec:", "sync:", "jdwp:", "track-jdwp", "track-app", "abb:", "abb_exec:", "localabstract:", "localfilesystem:", "localreserved:", "tcp:", "dev:", "framebuffer:", "reboot:", "root:", "unroot:", "remount:", "backup:", "restore:"} {
		if strings.HasPrefix(service, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) adbRelay(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		s.mu.Unlock()
		return
	}
	if d.adbStreams >= 65 {
		s.mu.Unlock()
		fail(w, 429, "too many ADB streams")
		return
	}
	d.adbStreams++
	l := d.lease
	s.mu.Unlock()
	defer func() { s.mu.Lock(); d.adbStreams--; s.mu.Unlock() }()
	upgrader := websocket.Upgrader{CheckOrigin: s.sameOrigin, ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopLease := context.AfterFunc(l.ctx, cancel)
	defer stopLease()
	go func() {
		defer logPanic("watch ADB relay")
		select {
		case <-l.done:
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "device access ended"), time.Now().Add(time.Second))
		case <-ctx.Done():
		}
		cancel()
		ws.Close()
	}()
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(8 * time.Second))
	var request struct {
		Service string `json:"service"`
	}
	if err := ws.ReadJSON(&request); err != nil {
		return
	}
	_ = ws.SetReadDeadline(time.Time{})
	if request.Service == "watch" {
		query, done := context.WithTimeout(ctx, 5*time.Second)
		features, err := s.cfg.ADB.Run(query, l.target.args("features")...)
		done()
		if err != nil {
			_ = ws.WriteJSON(map[string]string{"error": "cannot read device ADB features"})
			return
		}
		if err := ws.WriteJSON(map[string]any{"ok": true, "features": strings.TrimSpace(string(features))}); err != nil {
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}
	if strings.HasPrefix(request.Service, "reverse:") {
		_ = ws.WriteJSON(map[string]string{"error": "ADB reverse is unavailable through this relay"})
		return
	}
	if !deviceService(request.Service) {
		_ = ws.WriteJSON(map[string]string{"error": "only device ADB services are allowed"})
		return
	}
	provider, ok := s.cfg.ADB.(serviceProvider)
	if !ok {
		_ = ws.WriteJSON(map[string]string{"error": "ADB relay unavailable on this agent"})
		return
	}
	conn, err := provider.OpenService(ctx, l.target, request.Service)
	if err != nil {
		_ = ws.WriteJSON(map[string]string{"error": "cannot open device ADB service"})
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if ctx.Err() != nil {
		return
	}
	if err := ws.WriteJSON(map[string]bool{"ok": true}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		defer logPanic("read ADB relay")
		buffer := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if ws.WriteMessage(websocket.BinaryMessage, buffer[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		kind, reader, err := ws.NextReader()
		if err != nil {
			break
		}
		if kind != websocket.BinaryMessage {
			break
		}
		if _, err := io.Copy(conn, reader); err != nil {
			break
		}
	}
	cancel()
	conn.Close()
	<-done
}
