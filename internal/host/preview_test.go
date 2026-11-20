package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"marionette/internal/identity"
)

var operator = identity.User{Subject: "operator-1", Name: "Alice", Role: "user"}
var administrator = identity.User{Subject: "admin-1", Email: "admin@example.com", Role: "admin"}

func identityRequest(s *Server, user identity.User, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost"
	r.Header.Set("X-Lease-Token", token)
	r = r.WithContext(identity.WithUser(r.Context(), user))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func ownedToken(t *testing.T, s *Server) string {
	t.Helper()
	w := identityRequest(s, operator, "POST", "/api/devices/phone/lease", "", `{"owner":"spoofed"}`)
	var l lease
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &l) != nil || l.Owner != "Alice" {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	return l.Token
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}

// Test-only identity injection stands in for the separate OIDC middleware.
func socketServer(t *testing.T, s *Server, ctx context.Context, user identity.User) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r.WithContext(identity.WithUser(ctx, user)))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func dialStream(t *testing.T, ts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	ws, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+path, nil)
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, response)
	}
	t.Cleanup(func() { ws.Close() })
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, b, err := ws.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || b[0] != 1 {
		t.Fatalf("metadata: %d %x %v", kind, b, err)
	}
	return ws
}

func assertClosed(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatal("stream remained open")
			}
			return
		}
	}
}

func TestPreviewRoleAndSubjectAuthorization(t *testing.T) {
	s, _, _ := testServer(t)
	for _, user := range []identity.User{{}, operator} {
		w := identityRequest(s, user, "GET", "/api/admin/devices/phone/preview", "", "")
		if w.Code != 403 {
			t.Fatalf("preview permission: %d", w.Code)
		}
	}
	token := ownedToken(t, s)
	for _, user := range []identity.User{{}, administrator, {Subject: "operator-2", Role: "user"}} {
		for _, action := range []struct{ method, path, body string }{
			{"POST", "start", ""}, {"POST", "actions", `{"action":"info"}`},
			{"PUT", "lease", `{}`}, {"DELETE", "lease", ""},
			{"GET", "video?token=" + token, ""},
		} {
			w := identityRequest(s, user, action.method, "/api/devices/phone/"+action.path, token, action.body)
			if w.Code != 403 {
				t.Fatalf("subject %q %s: %d", user.Subject, action.path, w.Code)
			}
		}
	}
}

func TestAdminKeyframeRequiresRoleAndExistingRunner(t *testing.T) {
	s, adb, _ := testServer(t)
	path := "/api/admin/devices/phone/keyframe"
	for _, user := range []identity.User{{}, operator} {
		if w := identityRequest(s, user, "POST", path, "", ""); w.Code != 403 {
			t.Fatalf("permission: %d", w.Code)
		}
	}
	if w := identityRequest(s, administrator, "POST", path, "", ""); w.Code != 409 {
		t.Fatalf("idle: %d", w.Code)
	}
	if w := identityRequest(s, administrator, "POST", "/api/admin/devices/unknown/keyframe", "", ""); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	adb.mu.Lock()
	starts := len(adb.starts)
	adb.mu.Unlock()
	if starts != 0 {
		t.Fatal("keyframe started a runner")
	}
	ws := dialStream(t, socketServer(t, s, context.Background(), administrator), "/api/admin/devices/phone/preview")
	defer ws.Close()
	adb.mu.Lock()
	process := adb.process
	adb.mu.Unlock()
	count := func() int {
		process.mu.Lock()
		defer process.mu.Unlock()
		n := 0
		for _, a := range process.actions {
			if a["action"] == "keyframe" {
				n++
			}
		}
		return n
	}
	eventually(t, func() bool { return count() > 0 })
	before := count()
	w := identityRequest(s, administrator, "POST", path, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("keyframe: %d %s", w.Code, w.Body)
	}
	eventually(t, func() bool { return count() > before })
	s.mu.Lock()
	occupied := s.devices["phone"].lease != nil
	s.mu.Unlock()
	if occupied {
		t.Fatal("keyframe acquired control")
	}
}

func TestPreviewSharesRunnerAndSurvivesLeaseRelease(t *testing.T) {
	s, adb, _ := testServer(t)
	adminCtx, cancelAdmin := context.WithCancel(context.Background())
	defer cancelAdmin()
	ts := socketServer(t, s, adminCtx, administrator)
	preview := dialStream(t, ts, "/api/admin/devices/phone/preview")
	s.mu.Lock()
	initial := s.devices["phone"].runner
	occupied := s.devices["phone"].lease != nil
	profile := s.devices["phone"].profile
	s.mu.Unlock()
	if occupied || profile != "preview" {
		t.Fatal("preview acquired a lease or wrong profile")
	}
	adb.mu.Lock()
	script := strings.Join(adb.starts[0], " ")
	adb.mu.Unlock()
	if !strings.Contains(script, "--max-size 480 --max-fps 8 --bit-rate 600000") {
		t.Fatal(script)
	}
	token := ownedToken(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := identityRequest(s, operator, "POST", "/api/devices/phone/start", token, "")
			if w.Code != 200 {
				t.Errorf("start: %d %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	userTS := socketServer(t, s, context.Background(), operator)
	video := dialStream(t, userTS, "/api/devices/phone/video?token="+token)
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/actions", token, `{"action":"info"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := identityRequest(s, operator, "DELETE", "/api/devices/phone/lease", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	assertClosed(t, video)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.devices["phone"].profile == "preview"
	})
	s.mu.Lock()
	same := s.devices["phone"].runner == initial && initial.alive()
	s.mu.Unlock()
	if !same {
		t.Fatal("lease release replaced/stopped preview runner")
	}
	adb.mu.Lock()
	starts := len(adb.starts)
	p := adb.process
	adb.mu.Unlock()
	if starts != 1 {
		t.Fatalf("%d runners started", starts)
	}
	p.mu.Lock()
	var profiles []string
	for _, action := range p.actions {
		if action["action"] == "videoProfile" {
			profiles = append(profiles, action["profile"].(string))
		}
	}
	p.mu.Unlock()
	if strings.Join(profiles, ",") != "interactive,preview" {
		t.Fatalf("profile transitions: %v", profiles)
	}
	cancelAdmin()
	assertClosed(t, preview)
	eventually(t, func() bool { return !initial.alive() })
}

func TestSessionCancellationClosesOperatorStream(t *testing.T) {
	s, _, _ := testServer(t)
	token := ownedToken(t, s)
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/start", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := dialStream(t, socketServer(t, s, ctx, operator), "/api/devices/phone/video?token="+token)
	cancel()
	assertClosed(t, ws)
}

func TestMaintenanceRevokesOperatorVideoAndExpiryReturnsDevice(t *testing.T) {
	s, _, _ := testServer(t)
	const leasePath = "/api/devices/phone/lease"
	const modePath = "/api/admin/devices/phone/control-mode"
	claim := identityRequest(s, operator, "POST", leasePath, "", `{}`)
	var current lease
	if claim.Code != 201 || json.Unmarshal(claim.Body.Bytes(), &current) != nil {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body)
	}
	start := identityRequest(s, operator, "POST", "/api/devices/phone/start", current.Token, "")
	if start.Code != 200 {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	video := dialStream(t, socketServer(t, s, context.Background(), operator), "/api/devices/phone/video?token="+current.Token)
	body, err := json.Marshal(map[string]string{"mode": leaseMaintenance, "leaseId": current.ID})
	if err != nil {
		t.Fatal(err)
	}
	if w := identityRequest(s, administrator, "POST", modePath, "", string(body)); w.Code != 201 {
		t.Fatalf("maintenance: %d %s", w.Code, w.Body)
	}
	assertClosed(t, video)
	if w := identityRequest(s, operator, "POST", leasePath, "", `{}`); w.Code != 409 {
		t.Fatalf("device is not reserved for maintenance: %d %s", w.Code, w.Body)
	}
	forceExpiry := func() {
		s.mu.Lock()
		s.devices["phone"].lease.ExpiresAt = time.Now().Add(-time.Second)
		s.expireLocked()
		s.mu.Unlock()
	}
	forceExpiry()
	if w := identityRequest(s, administrator, "POST", modePath, "", `{"mode":"temporary","leaseId":""}`); w.Code != 201 {
		t.Fatalf("temporary control after maintenance expiry: %d %s", w.Code, w.Body)
	}
	forceExpiry()
	if w := identityRequest(s, operator, "POST", leasePath, "", `{}`); w.Code != 201 {
		t.Fatalf("claim after temporary expiry: %d %s", w.Code, w.Body)
	}
}

func TestPreviewDeparturePreservesActiveOperator(t *testing.T) {
	s, adb, _ := testServer(t)
	token := ownedToken(t, s)
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/start", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	s.mu.Lock()
	runner := s.devices["phone"].runner
	s.mu.Unlock()
	preview := dialStream(t, socketServer(t, s, context.Background(), administrator), "/api/admin/devices/phone/preview")
	preview.Close()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devices["phone"].previews == 0 })
	s.mu.Lock()
	preserved := s.devices["phone"].runner == runner && s.devices["phone"].lease.Token == token && s.devices["phone"].profile == "interactive"
	s.mu.Unlock()
	if !preserved || !runner.alive() {
		t.Fatal("preview departure altered operator runtime/lease")
	}
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/actions", token, `{"action":"info"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	adb.mu.Lock()
	starts := len(adb.starts)
	adb.mu.Unlock()
	if starts != 1 {
		t.Fatalf("started %d runtimes", starts)
	}
}

func TestLeaseExpiryDowngradesWithoutClosingPreview(t *testing.T) {
	s, _, _ := testServer(t)
	token := ownedToken(t, s)
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/start", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	preview := dialStream(t, socketServer(t, s, context.Background(), administrator), "/api/admin/devices/phone/preview")
	defer preview.Close()
	s.mu.Lock()
	runner := s.devices["phone"].runner
	s.devices["phone"].lease.ExpiresAt = time.Now().Add(-time.Second)
	s.expireLocked()
	s.mu.Unlock()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devices["phone"].profile == "preview" })
	if !runner.alive() {
		t.Fatal("expiry closed preview runtime")
	}
	if w := identityRequest(s, operator, "POST", "/api/devices/phone/actions", token, `{"action":"info"}`); w.Code != 403 {
		t.Fatal("expired lease retained control")
	}
}

func TestPreviewDisconnectAndServerClose(t *testing.T) {
	for _, reason := range []string{"disconnect", "shutdown", "last-viewer"} {
		t.Run(reason, func(t *testing.T) {
			s, adb, _ := testServer(t)
			ws := dialStream(t, socketServer(t, s, context.Background(), administrator), "/api/admin/devices/phone/preview")
			s.mu.Lock()
			runner := s.devices["phone"].runner
			s.mu.Unlock()
			switch reason {
			case "disconnect":
				adb.mu.Lock()
				adb.listing = ""
				adb.mu.Unlock()
				s.discover(context.Background())
			case "shutdown":
				s.Close()
			case "last-viewer":
				ws.Close()
			}
			if reason != "last-viewer" {
				assertClosed(t, ws)
			}
			eventually(t, func() bool { return !runner.alive() })
		})
	}
}

func TestPublicOriginAndNoFrontendRoutes(t *testing.T) {
	s := New(Config{PublicOrigin: "https://farm.example.com"})
	defer s.Close()
	for _, tc := range []struct {
		host, origin string
		status       int
	}{
		{"farm.example.com", "https://farm.example.com", 200},
		{"farm.example.com", "http://farm.example.com", 403},
		{"attacker.example", "https://farm.example.com", 403},
		{"localhost", "https://farm.example.com", 403},
		{"localhost", "http://localhost", 200},
	} {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Host", "farm.example.com")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("origin %+v: %d", tc, w.Code)
		}
	}
	for _, path := range []string{"/admin", "/devices", "/login"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := request(t, s, method, path, "", "")
			if w.Code != 404 {
				t.Fatalf("agent served frontend route %s %s: %d", method, path, w.Code)
			}
		}
	}
	if w := request(t, s, "GET", "/missing.js", "", ""); w.Code != 404 {
		t.Fatal("missing asset became SPA")
	}
}

type gatedADB struct {
	*fakeADB
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (f *gatedADB) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 2 && args[2] == "push" {
		f.once.Do(func() { close(f.entered) })
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeADB.Run(ctx, args...)
}

func TestPreviewAndOperatorShareColdStartup(t *testing.T) {
	s, adb, _ := testServer(t)
	gate := &gatedADB{fakeADB: adb, entered: make(chan struct{}), gate: make(chan struct{})}
	s.cfg.ADB = gate
	ts := socketServer(t, s, context.Background(), administrator)
	type result struct {
		ws  *websocket.Conn
		err error
	}
	previewDone := make(chan result, 1)
	go func() {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/admin/devices/phone/preview", nil)
		previewDone <- result{ws, err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not begin")
	}
	token := ownedToken(t, s)
	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { startDone <- identityRequest(s, operator, "POST", "/api/devices/phone/start", token, "") }()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.devices["phone"].interactive })
	close(gate.gate)
	p := <-previewDone
	if p.err != nil {
		t.Fatal(p.err)
	}
	defer p.ws.Close()
	w := <-startDone
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	s.mu.Lock()
	profile := s.devices["phone"].profile
	s.mu.Unlock()
	adb.mu.Lock()
	starts := len(adb.starts)
	adb.mu.Unlock()
	if profile != "interactive" || starts != 1 {
		t.Fatalf("profile=%s starts=%d", profile, starts)
	}
}

func TestCloseCancelsPreviewStartup(t *testing.T) {
	s, adb, _ := testServer(t)
	gate := &gatedADB{fakeADB: adb, entered: make(chan struct{}), gate: make(chan struct{})}
	s.cfg.ADB = gate
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		identityRequest(s, administrator, "GET", "/api/admin/devices/phone/preview", "", "")
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not begin")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel startup")
	}
	<-requestDone
	adb.mu.Lock()
	starts := len(adb.starts)
	adb.mu.Unlock()
	if starts != 0 {
		t.Fatal("process started after shutdown")
	}
}

func TestRapidRediscoveryNeverOverlapsDeviceRuntimes(t *testing.T) {
	s, adb, _ := testServer(t)
	ts := socketServer(t, s, context.Background(), administrator)
	first := dialStream(t, ts, "/api/admin/devices/phone/preview")
	adb.mu.Lock()
	adb.listing = ""
	adb.mu.Unlock()
	s.discover(context.Background())
	// Reappearance may race the asynchronous old runner cleanup.
	adb.mu.Lock()
	adb.listing = "phone device model:Pixel\n"
	adb.mu.Unlock()
	s.discover(context.Background())
	next := dialStream(t, ts, "/api/admin/devices/phone/preview")
	defer next.Close()
	assertClosed(t, first)
	adb.mu.Lock()
	overlaps := adb.overlappingStarts
	starts := len(adb.starts)
	adb.mu.Unlock()
	if overlaps != 0 || starts != 2 {
		t.Fatalf("starts=%d overlapping=%d", starts, overlaps)
	}
}
