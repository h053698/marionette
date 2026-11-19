package backend_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"marionette/internal/agent"
	"marionette/internal/backend"
	"marionette/internal/host"
	"marionette/internal/identity"
	"marionette/internal/tunnel"
)

const token = "test-agent-token-at-least-32-bytes-long"

type harness struct {
	app           *backend.Server
	server        *httptest.Server
	cancelSession context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	app, err := backend.New(map[string]string{"desk-a": token, "desk-b": token})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	mux.HandleFunc("GET /internal/agents/{id}/connect", app.Connect)
	// Identity is supplied here in place of the separately tested OIDC wrapper.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(sessionCtx, cancel)
		defer stop()
		defer cancel()
		subject := r.Header.Get("Test-Subject")
		if subject == "" {
			subject = "alice"
		}
		role := r.Header.Get("Test-Role")
		if role == "" {
			role = "user"
		}
		app.ServeHTTP(w, r.WithContext(identity.WithUser(ctx, identity.User{Subject: subject, Name: subject, Role: role})))
	}))
	h := &harness{app: app, server: httptest.NewServer(mux), cancelSession: cancelSession}
	t.Cleanup(func() { cancelSession(); app.Close(); h.server.Close() })
	return h
}

func (h *harness) startAgent(t *testing.T, id string, handler http.Handler) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{BackendURL: h.server.URL, ID: id, Token: token}, func(ctx context.Context, listener net.Listener) error {
			server := &http.Server{Handler: agent.WithIdentity(handler), BaseContext: func(net.Listener) context.Context { return ctx }}
			stop := context.AfterFunc(ctx, func() { server.Close() })
			defer stop()
			defer server.Close()
			return server.Serve(listener)
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("agent failed to stop")
		}
	})
	return cancel
}

func (h *harness) request(t *testing.T, method, path, body string, headers http.Header) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if headers != nil {
		r.Header = headers
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func waitDevices(t *testing.T, h *harness, count int) []map[string]any {
	t.Helper()
	var body struct {
		Devices []map[string]any `json:"devices"`
	}
	eventually(t, func() bool {
		_, data := h.request(t, "GET", "/api/devices", "", http.Header{"Test-Role": {"admin"}})
		return json.Unmarshal(data, &body) == nil && len(body.Devices) == count
	})
	return body.Devices
}

var videoPacket = bytes.Repeat([]byte{0, 1, 2, 3, 4, 255}, 350000)

func fakeDeviceHandler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			fmt.Fprint(w, `{"adbAvailable":true,"deviceJarAvailable":true}`)
		case "/api/devices", "/api/assignments":
			user, _ := identity.FromContext(r.Context())
			mine := id == "desk-a" && user.Subject == "alice" || id == "desk-b" && user.Subject == "bob"
			if !mine && (r.URL.Path == "/api/assignments" || !identity.IsAdmin(r.Context())) {
				fmt.Fprint(w, `{"devices":[]}`)
				return
			}
			fmt.Fprintf(w, `{"devices":[{"serial":"emulator-5554","state":"device","mine":%t}]}`, mine)
		case "/api/devices/emulator-5554/video":
			ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			_ = ws.WriteMessage(websocket.BinaryMessage, videoPacket)
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		default:
			user, _ := identity.FromContext(r.Context())
			data, _ := io.ReadAll(r.Body)
			json.NewEncoder(w).Encode(map[string]any{"agent": id, "path": r.URL.Path, "user": user, "authorization": r.Header.Get("Authorization"), "cookie": r.Header.Get("Cookie"), "body": string(data), "lease": r.Header.Get("X-Lease-Token")})
		}
	})
}

func TestMultipleAgentsRouteAndPreserveTrustedIdentity(t *testing.T) {
	h := newHarness(t)
	h.startAgent(t, "desk-a", fakeDeviceHandler("desk-a"))
	h.startAgent(t, "desk-b", fakeDeviceHandler("desk-b"))
	devices := waitDevices(t, h, 2)
	if devices[0]["serial"] == devices[1]["serial"] {
		t.Fatal("duplicate ADB serials collided")
	}
	spoof, _ := json.Marshal(identity.User{Subject: "mallory", Role: "admin"})
	for id, subject := range map[string]string{"desk-a": "alice", "desk-b": "bob"} {
		path := "/api/devices/" + backend.DeviceID(id, "emulator-5554") + "/actions"
		status, data := h.request(t, "POST", path, `{"action":"tap","x":42}`, http.Header{
			agent.IdentityHeader: {base64.RawURLEncoding.EncodeToString(spoof)}, "Authorization": {"Bearer personal-secret"}, "Cookie": {"session=secret"}, "X-Lease-Token": {"lease-secret"}, "Test-Subject": {subject},
		})
		var result struct {
			Agent, Path, Authorization, Cookie, Body, Lease string
			User                                            identity.User
		}
		if status != 200 || json.Unmarshal(data, &result) != nil {
			t.Fatalf("proxy: %d %s", status, data)
		}
		if result.Agent != id || result.Path != "/api/devices/emulator-5554/actions" || result.User.Subject != subject || result.User.Role != "user" || result.Authorization != "" || result.Cookie != "" || result.Lease != "lease-secret" || result.Body != `{"action":"tap","x":42}` {
			t.Fatalf("proxy did not preserve boundary: %+v", result)
		}
	}
	_, data := h.request(t, "GET", "/api/status", "", http.Header{"Test-Role": {"admin"}})
	if !bytes.Contains(data, []byte(`"agentCount":2`)) {
		t.Fatalf("status: %s", data)
	}
}

func TestVideoPassesThroughRelayAndClosesWithSession(t *testing.T) {
	h := newHarness(t)
	cancelAgent := h.startAgent(t, "desk-a", fakeDeviceHandler("desk-a"))
	waitDevices(t, h, 1)
	u := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/devices/" + backend.DeviceID("desk-a", "emulator-5554") + "/video?token=lease"
	ws, response, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("video handshake: %v (%v)", err, response)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := ws.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(data, videoPacket) {
		t.Fatalf("video changed: kind=%d bytes=%d error=%v", kind, len(data), err)
	}
	cancelAgent()
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("stream survived agent disconnect")
	}
	waitDevices(t, h, 0)
	status, _ := h.request(t, "POST", "/api/devices/"+backend.DeviceID("desk-a", "emulator-5554")+"/start", "", http.Header{"Test-Role": {"admin"}})
	if status != 503 {
		t.Fatalf("offline agent: %d", status)
	}
}

func TestSessionCancellationClosesRelayedVideo(t *testing.T) {
	h := newHarness(t)
	h.startAgent(t, "desk-a", fakeDeviceHandler("desk-a"))
	waitDevices(t, h, 1)
	u := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/devices/" + backend.DeviceID("desk-a", "emulator-5554") + "/video?token=lease"
	ws, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	h.cancelSession()
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("video survived authentication session cancellation")
	}
}

type discoveryADB struct{}

func (discoveryADB) Run(context.Context, ...string) ([]byte, error) {
	return []byte("List of devices attached\nemulator-5554 device model:Phone\n"), nil
}
func (discoveryADB) Start(context.Context, ...string) (host.Process, error) {
	return nil, fmt.Errorf("unexpected runtime start")
}

func TestLeaseOwnershipAndAdministratorPolicyAcrossRelay(t *testing.T) {
	h := newHarness(t)
	runtime := host.New(host.Config{ADB: discoveryADB{}})
	runtime.Start(context.Background())
	defer runtime.Close()
	h.startAgent(t, "desk-a", runtime)
	waitDevices(t, h, 1)
	status, data := h.request(t, "GET", "/api/devices", "", nil)
	if status != 200 || !bytes.Contains(data, []byte(`"devices":[]`)) {
		t.Fatalf("unassigned member inventory: %d %s", status, data)
	}
	path := "/api/devices/" + backend.DeviceID("desk-a", "emulator-5554")
	status, data = h.request(t, "POST", path+"/lease", `{"owner":"spoofed"}`, nil)
	var lease struct{ ID, Token, Owner string }
	if status != 201 || json.Unmarshal(data, &lease) != nil || lease.Owner != "alice" {
		t.Fatalf("claim: %d %s", status, data)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		status, _ = h.request(t, method, path+"/lease", `{}`, http.Header{"X-Lease-Token": {lease.Token}, "Test-Subject": {"bob"}})
		if status != 403 {
			t.Fatalf("other identity used lease: %d", status)
		}
	}
	adminPath := "/api/admin/devices/" + backend.DeviceID("desk-a", "emulator-5554") + "/lease"
	status, _ = h.request(t, "DELETE", adminPath, `{"leaseId":"`+lease.ID+`"}`, nil)
	if status != 403 {
		t.Fatalf("member used admin API: %d", status)
	}
	status, _ = h.request(t, "DELETE", adminPath, `{"leaseId":"`+lease.ID+`"}`, http.Header{"Test-Role": {"admin"}})
	if status != 200 {
		t.Fatalf("admin could not disconnect: %d", status)
	}
	status, _ = h.request(t, "PUT", path+"/lease", `{}`, http.Header{"X-Lease-Token": {lease.Token}})
	if status != 403 {
		t.Fatalf("revoked lease survived: %d", status)
	}
}

func TestAgentCredentialAndDuplicateConnection(t *testing.T) {
	h := newHarness(t)
	dialer := websocket.Dialer{Subprotocols: []string{tunnel.Protocol}}
	u := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/internal/agents/desk-a/connect"
	for _, headers := range []http.Header{nil, {"Authorization": {"Bearer wrong"}}, {"Authorization": {"Bearer " + token}, "Origin": {"https://attacker.invalid"}}} {
		ws, response, err := dialer.Dial(u, headers)
		if ws != nil {
			ws.Close()
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != 401 {
			t.Fatalf("invalid agent accepted: %v %v", response, err)
		}
	}
	h.startAgent(t, "desk-a", fakeDeviceHandler("desk-a"))
	waitDevices(t, h, 1)
	ws, response, err := dialer.Dial(u, http.Header{"Authorization": {"Bearer " + token}})
	if ws != nil {
		ws.Close()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatalf("duplicate agent accepted: %v %v", response, err)
	}
}
