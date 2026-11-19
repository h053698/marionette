package host

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeADB struct {
	mu                sync.Mutex
	listing           string
	listener          net.Listener
	process           *fakeProcess
	calls             [][]string
	captureError      string
	starts            [][]string
	overlappingStarts int
}

func (f *fakeADB) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if args[0] == "devices" {
		return []byte(f.listing), nil
	}
	if len(args) > 3 && args[2] == "forward" && args[3] == "tcp:0" {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return nil, e
		}
		f.listener = l
		return []byte(fmt.Sprint(l.Addr().(*net.TCPAddr).Port)), nil
	}
	return nil, nil
}
func (f *fakeADB) Start(ctx context.Context, args ...string) (Process, error) {
	f.mu.Lock()
	f.starts = append(f.starts, append([]string(nil), args...))
	if f.process != nil {
		select {
		case <-f.process.done:
		default:
			f.overlappingStarts++
		}
	}
	p := &fakeProcess{done: make(chan struct{}), l: f.listener, captureError: f.captureError}
	f.process = p
	f.mu.Unlock()
	go p.serve()
	return p, nil
}

type fakeProcess struct {
	mu           sync.Mutex
	once         sync.Once
	done         chan struct{}
	l            net.Listener
	conns        []net.Conn
	captureError string
	actions      []map[string]any
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Stop() error {
	p.once.Do(func() {
		close(p.done)
		p.l.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			c.Close()
		}
	})
	return nil
}
func packet(kind byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	copy(b[5:], payload)
	return b
}
func (p *fakeProcess) serve() {
	for {
		c, e := p.l.Accept()
		if e != nil {
			return
		}
		p.mu.Lock()
		select {
		case <-p.done:
			c.Close()
			p.mu.Unlock()
			return
		default:
		}
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		go func() {
			var selector [1]byte
			if _, e := io.ReadFull(c, selector[:]); e != nil {
				return
			}
			if selector[0] == 1 {
				if p.captureError != "" {
					payload, _ := json.Marshal(map[string]string{"error": p.captureError})
					_, _ = c.Write(packet(4, payload))
					<-p.done
					return
				}
				_, _ = c.Write(packet(1, []byte(`{"width":720,"height":1280,"codec":"avc1.42E01F"}`)))
				_, _ = c.Write(packet(2, []byte{0, 0, 0, 1, 0x67}))
				<-p.done
				return
			}
			d := json.NewDecoder(c)
			for {
				var request map[string]any
				if d.Decode(&request) != nil {
					return
				}
				p.mu.Lock()
				p.actions = append(p.actions, request)
				p.mu.Unlock()
				_ = json.NewEncoder(c).Encode(map[string]any{"id": request["id"], "ok": true, "result": request["action"]})
			}
		}()
	}
}
func testServer(t *testing.T) (*Server, *fakeADB, *httptest.Server) {
	t.Helper()
	adb := &fakeADB{listing: "List of devices attached\nphone device model:Pixel\nlocked unauthorized\nmissing offline\n"}
	s := New(Config{ADB: adb, DeviceJar: "unused.jar"})
	s.discover(context.Background())
	httpServer := httptest.NewServer(s)
	t.Cleanup(func() { s.Close(); httpServer.Close() })
	return s, adb, httpServer
}
func request(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost"
	r.Header.Set("X-Lease-Token", token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func claimToken(t *testing.T, s *Server) string {
	t.Helper()
	w := request(t, s, "POST", "/api/devices/phone/lease", "", `{"owner":"tester"}`)
	if w.Code != 201 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var l lease
	if e := json.Unmarshal(w.Body.Bytes(), &l); e != nil {
		t.Fatal(e)
	}
	return l.Token
}
func TestLeaseRaceSingleWinner(t *testing.T) {
	s, _, _ := testServer(t)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(t, s, "POST", "/api/devices/phone/lease", "", `{"owner":"racer"}`)
			if w.Code == 201 {
				winners.Add(1)
			} else if w.Code != 409 {
				t.Errorf("unexpected claim status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners = %d", winners.Load())
	}
}
func TestLeaseReleaseExpiryAndAuthorization(t *testing.T) {
	s, _, _ := testServer(t)
	token := claimToken(t, s)
	for _, path := range []string{"start", "actions"} {
		w := request(t, s, "POST", "/api/devices/phone/"+path, "wrong", `{"action":"info"}`)
		if w.Code != 403 {
			t.Fatalf("%s unauthorized: %d", path, w.Code)
		}
	}
	if w := request(t, s, "DELETE", "/api/devices/phone/lease", token, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	newToken := claimToken(t, s)
	if newToken == token {
		t.Fatal("token reused")
	}
	if w := request(t, s, "PUT", "/api/devices/phone/lease", token, `{}`); w.Code != 403 {
		t.Fatal("old token accepted")
	}
	s.mu.Lock()
	s.devices["phone"].lease.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if w := request(t, s, "POST", "/api/devices/phone/start", newToken, ""); w.Code != 403 {
		t.Fatal("expired token accepted")
	}
	for _, serial := range []string{"locked", "missing"} {
		if w := request(t, s, "POST", "/api/devices/"+serial+"/lease", "", `{"owner":"x"}`); w.Code != 409 {
			t.Fatalf("leased unavailable device: %d", w.Code)
		}
	}
}
func TestPacketBoundsAndTruncation(t *testing.T) {
	for name, b := range map[string][]byte{"header": {1, 0}, "payload": {1, 0, 0, 0, 2, 1}, "oversize": {1, 1, 0, 0, 1}, "shortFrame": {3, 0, 0, 0, 1, 0}, "emptyAVC": packet(3, make([]byte, 9)), "unknown": {9, 0, 0, 0, 0}} {
		t.Run(name, func(t *testing.T) {
			if _, e := ReadPacket(bytes.NewReader(b)); e == nil {
				t.Fatal("accepted invalid wire packet")
			}
		})
	}
	want := packet(3, append(make([]byte, 8), 1, 0, 0, 1, 0x65))
	got, e := ReadPacket(bytes.NewReader(want))
	if e != nil || !bytes.Equal(got, want) || !keyframe(got) {
		t.Fatalf("valid packet: %x %v", got, e)
	}
}
func TestRunnerActionsAndStreamInvalidation(t *testing.T) {
	for _, reason := range []string{"release", "expiry", "disconnect"} {
		t.Run(reason, func(t *testing.T) {
			s, adb, ts := testServer(t)
			token := claimToken(t, s)
			if w := request(t, s, "POST", "/api/devices/phone/start", token, ""); w.Code != 200 {
				t.Fatalf("start: %s", w.Body)
			}
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w := request(t, s, "POST", "/api/devices/phone/actions", token, `{"action":"info","id":"untrusted"}`)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
						t.Errorf("action: %d %s", w.Code, w.Body)
					}
				}()
			}
			wg.Wait()
			u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/devices/phone/video?token=" + token
			bad := http.Header{"Origin": []string{"http://attacker.invalid"}}
			if c, response, e := websocket.DefaultDialer.Dial(u, bad); e == nil {
				c.Close()
				t.Fatal("cross-origin websocket accepted")
			} else if response.StatusCode != 403 {
				t.Fatalf("origin status %d", response.StatusCode)
			}
			ws, _, e := websocket.DefaultDialer.Dial(u, http.Header{"Origin": []string{ts.URL}})
			if e != nil {
				t.Fatal(e)
			}
			defer ws.Close()
			_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
			kind, b, e := ws.ReadMessage()
			if e != nil || kind != websocket.BinaryMessage || b[0] != 1 {
				t.Fatalf("metadata: %d %x %v", kind, b, e)
			}
			switch reason {
			case "release":
				request(t, s, "DELETE", "/api/devices/phone/lease", token, "")
			case "expiry":
				s.mu.Lock()
				s.devices["phone"].lease.ExpiresAt = time.Now().Add(-time.Second)
				s.expireLocked()
				s.mu.Unlock()
			case "disconnect":
				adb.mu.Lock()
				adb.listing = "List of devices attached\n"
				adb.mu.Unlock()
				s.discover(context.Background())
			}
			for {
				_, _, e = ws.ReadMessage()
				if e != nil {
					break
				}
			}
			if ne, ok := e.(net.Error); ok && ne.Timeout() {
				t.Fatal("stream remained open after invalidation")
			}
		})
	}
}
func TestDiscoveryStatesAndOrigin(t *testing.T) {
	s, _, _ := testServer(t)
	w := request(t, s, "GET", "/api/devices", "", "")
	for _, state := range []string{"unauthorized", "offline", "device"} {
		if !strings.Contains(w.Body.String(), `"state":"`+state+`"`) {
			t.Fatalf("missing %s: %s", state, w.Body)
		}
	}
	r := httptest.NewRequest("POST", "/api/devices/phone/lease", strings.NewReader(`{"owner":"x"}`))
	r.Host = "localhost"
	r.Header.Set("Origin", "https://evil.invalid")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin claim: %d", w.Code)
	}
}

func TestSlowViewerResumesOnlyAtKeyframe(t *testing.T) {
	v := &viewer{packets: make(chan []byte, 16), waiting: true}
	r := &Runner{viewers: map[*viewer]struct{}{v: {}}, keyframes: make(chan struct{}, 1)}
	metadata := packet(1, []byte(`{"width":720}`))
	config := packet(2, []byte{0, 0, 1, 0x67})
	delta := packet(3, append(make([]byte, 9), 0x41))
	key := packet(3, append(make([]byte, 8), 1, 0x65))
	r.broadcast(metadata)
	r.broadcast(config)
	r.broadcast(delta)
	if len(v.packets) != 2 {
		t.Fatal("new viewer received dependent frame")
	}
	r.broadcast(key)
	for i := 0; i < 30; i++ {
		r.broadcast(delta)
	}
	if !v.waiting {
		t.Fatal("overflow did not suspend dependent frames")
	}
	if len(r.keyframes) != 1 {
		t.Fatal("overflow did not request an IDR")
	}
	if len(v.packets) != 2 {
		t.Fatalf("overflow should retain only decoder context, got %d packets", len(v.packets))
	}
	r.broadcast(key)
	for i, want := range [][]byte{metadata, config, key} {
		select {
		case got := <-v.packets:
			if !bytes.Equal(got, want) {
				t.Fatalf("packet %d incorrect", i)
			}
		default:
			t.Fatal("missing resumption packet")
		}
	}
	if v.waiting {
		t.Fatal("keyframe did not resume viewer")
	}
}

func TestHostAllowlist(t *testing.T) {
	s, _, _ := testServer(t)
	for _, host := range []string{"localhost", "LOCALHOST:8080", "127.0.0.1:8080", "127.12.34.56", "[::1]", "[::1]:8080", "[::ffff:127.0.0.1]:8080"} {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = host
		r.Header.Set("Origin", "http://"+host)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("loopback Host %q rejected: %d", host, w.Code)
		}
	}
	for _, host := range []string{"rebind.attacker.test:8080", "localhost.attacker.test", "192.168.1.2:8080", "[2001:db8::1]:8080", "localhost:bad", "localhost:", "localhost:99999", "", "127.1:8080"} {
		for _, origin := range []string{"", "http://" + host} {
			r := httptest.NewRequest("GET", "/api/status", nil)
			r.Host = host
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Errorf("untrusted Host %q with Origin %q accepted", host, origin)
			}
		}
	}
}

func TestRotationDiscardsOldDecoderState(t *testing.T) {
	v := &viewer{packets: make(chan []byte, 16), waiting: true}
	r := &Runner{viewers: map[*viewer]struct{}{v: {}}, keyframes: make(chan struct{}, 1)}
	oldMeta := packet(1, []byte(`{"width":720,"height":1280}`))
	newMeta := packet(1, []byte(`{"width":1280,"height":720}`))
	oldConfig := packet(2, []byte{0, 0, 1, 0x67, 1})
	newConfig := packet(2, []byte{0, 0, 1, 0x67, 2})
	key := packet(3, append(make([]byte, 8), 1, 0x65))
	delta := packet(3, append(make([]byte, 9), 0x41))
	for _, b := range [][]byte{oldMeta, oldConfig, key, delta, newMeta} {
		r.broadcast(b)
	}
	if r.config != nil || len(v.packets) != 1 {
		t.Fatal("rotation retained old config or queued frames")
	}
	joining := r.subscribe()
	defer r.unsubscribe(joining)
	if len(joining.packets) != 1 {
		t.Fatal("new viewer inherited stale codec config")
	}
	r.broadcast(key)
	if len(v.packets) != 1 || len(joining.packets) != 1 {
		t.Fatal("frame accepted before new codec configuration")
	}
	r.broadcast(newConfig)
	r.broadcast(delta)
	r.broadcast(key)
	for _, client := range []*viewer{v, joining} {
		for i, want := range [][]byte{newMeta, newConfig, key} {
			select {
			case got := <-client.packets:
				if !bytes.Equal(got, want) {
					t.Fatalf("rotation packet %d incorrect", i)
				}
			default:
				t.Fatal("missing new decoder state")
			}
		}
		if client.waiting {
			t.Fatal("viewer did not resume after rotation keyframe")
		}
	}
}

func TestCaptureStartupErrorCleansOnlyOwnedForward(t *testing.T) {
	adb := &fakeADB{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy, err := startRunner(ctx, adb, "phone", "device.jar")
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	adb.mu.Lock()
	adb.captureError = "capture denied by Android"
	adb.mu.Unlock()
	failed, err := startRunner(ctx, adb, "phone", "device.jar")
	if err == nil || !strings.Contains(err.Error(), "capture denied by Android") {
		t.Fatalf("capture error not surfaced: %v", err)
	}
	select {
	case <-failed.process.Done():
	default:
		t.Fatal("failed device process not stopped")
	}
	if !healthy.alive() {
		t.Fatal("cleanup stopped another runner")
	}
	adb.mu.Lock()
	defer adb.mu.Unlock()
	removed := 0
	for _, args := range adb.calls {
		if len(args) >= 4 && args[2] == "forward" && strings.HasPrefix(args[3], "--remove") {
			if len(args) != 5 || args[3] != "--remove" || args[4] != "tcp:"+failed.port {
				t.Fatalf("cleanup removed foreign forwarding: %v", args)
			}
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("expected one owned-forward removal, got %d", removed)
	}
}
