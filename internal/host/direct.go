package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"marionette/internal/direct"
)

const (
	maxDirectPeers    = 4
	maxDirectChannels = 64
	directIdleOpen    = 30 * time.Second
)

// directADB answers a WebRTC offer from the leaseholder's CLI. Only the
// offer and answer pass through the backend; ADB bytes then flow between the
// two peers. Every channel is bound to the lease that authorized the offer.
func (s *Server) directADB(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SDP string `json:"sdp"`
	}
	if err := readJSON(w, r, &request); err != nil || request.SDP == "" || len(request.SDP) > direct.MaxOffer {
		fail(w, 400, "invalid WebRTC offer")
		return
	}
	provider, ok := s.cfg.ADB.(serviceProvider)
	if !ok {
		fail(w, 503, "ADB relay unavailable on this agent")
		return
	}
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		s.mu.Unlock()
		return
	}
	if d.directPeers >= maxDirectPeers {
		s.mu.Unlock()
		fail(w, 429, "too many direct ADB connections")
		return
	}
	d.directPeers++
	l := d.lease
	s.mu.Unlock()
	release := sync.OnceFunc(func() { s.mu.Lock(); d.directPeers--; s.mu.Unlock() })

	pc, err := s.directAPI().NewPeerConnection(webrtc.Configuration{ICEServers: s.cfg.ICEServers})
	if err != nil {
		release()
		fail(w, 500, "cannot create WebRTC peer")
		return
	}
	// The peer outlives this request; it ends with the lease or the connection.
	ctx, cancel := context.WithCancel(l.ctx)
	var mu sync.Mutex
	seen, active, opened := 0, 0, false
	idle := time.AfterFunc(directIdleOpen, func() {
		mu.Lock()
		defer mu.Unlock()
		if !opened {
			cancel()
		}
	})
	go func() {
		defer release()
		defer logPanic("close direct ADB peer")
		<-ctx.Done()
		idle.Stop()
		_ = pc.Close()
	}()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			cancel()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		defer logPanic("accept direct ADB channel")
		mu.Lock()
		// Pion retains stream IDs for the peer's lifetime, including failed
		// or rejected channels. Count every event before inspecting its label.
		seen++
		if seen > maxDirectChannels || active >= maxDirectChannels || ctx.Err() != nil {
			mu.Unlock()
			cancel()
			return
		}
		active++
		mu.Unlock()
		releaseChannel := sync.OnceFunc(func() { mu.Lock(); active--; mu.Unlock() })
		dc.OnClose(releaseChannel)
		dc.OnError(func(error) { releaseChannel(); _ = dc.Close() })
		stop := context.AfterFunc(ctx, releaseChannel)
		if dc.Label() != "adb" {
			stop()
			releaseChannel()
			_ = dc.Close()
			return
		}
		dc.OnOpen(func() {
			defer logPanic("serve direct ADB channel")
			defer releaseChannel()
			defer stop()
			defer dc.Close()
			if ctx.Err() == nil {
				ch, err := direct.Open(dc)
				if err == nil {
					mu.Lock()
					opened = true
					mu.Unlock()
					idle.Stop()
					s.serveDirect(ctx, ch, provider, l.target)
				}
			}
		})
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: request.SDP}); err != nil {
		idle.Stop()
		cancel()
		fail(w, 400, "invalid WebRTC offer")
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(answer)
	}
	var sdp string
	if err == nil {
		sdp, err = direct.Gather(r.Context(), pc)
	}
	if err != nil {
		idle.Stop()
		cancel()
		fail(w, 500, "cannot answer WebRTC offer")
		return
	}
	writeJSON(w, 200, map[string]string{"sdp": sdp})
}

func (s *Server) directAPI() *webrtc.API {
	s.directOnce.Do(func() { s.direct = direct.NewAPI() })
	return s.direct
}

func (s *Server) serveDirect(ctx context.Context, ch *direct.Channel, provider serviceProvider, target adbTarget) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer ch.Close()
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	reply := func(v any) error {
		data, _ := json.Marshal(v)
		return ch.Write(ctx, data)
	}
	first, err := ch.Read(time.Now().Add(8 * time.Second))
	if err != nil {
		return
	}
	var request struct {
		Service string `json:"service"`
	}
	if json.Unmarshal(first, &request) != nil {
		return
	}
	select {
	case <-ctx.Done():
		_ = reply(map[string]string{"error": "device access ended"})
		return
	default:
	}
	if request.Service == "watch" {
		query, done := context.WithTimeout(ctx, 5*time.Second)
		features, err := s.cfg.ADB.Run(query, target.args("features")...)
		done()
		if err != nil {
			_ = reply(map[string]string{"error": "cannot read device ADB features"})
			return
		}
		if reply(map[string]any{"ok": true, "features": strings.TrimSpace(string(features))}) != nil {
			return
		}
		for {
			if _, err := ch.Read(time.Time{}); err != nil {
				return
			}
		}
	}
	if strings.HasPrefix(request.Service, "reverse:") {
		_ = reply(map[string]string{"error": "ADB reverse is unavailable through this relay"})
		return
	}
	if !deviceService(request.Service) {
		_ = reply(map[string]string{"error": "only device ADB services are allowed"})
		return
	}
	conn, err := provider.OpenService(ctx, target, request.Service)
	if err != nil {
		_ = reply(map[string]string{"error": "cannot open device ADB service"})
		return
	}
	defer conn.Close()
	closeConn := context.AfterFunc(ctx, func() { conn.Close() })
	defer closeConn()
	if reply(map[string]bool{"ok": true}) != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		defer logPanic("read direct ADB service")
		buffer := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buffer)
			if n > 0 && ch.Write(ctx, buffer[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		data, err := ch.Read(time.Time{})
		if err != nil {
			break
		}
		if _, err := conn.Write(data); err != nil {
			break
		}
	}
	cancel()
	conn.Close()
	<-done
}

var _ io.Closer = (*direct.Channel)(nil)
