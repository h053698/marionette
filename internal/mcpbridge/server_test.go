package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"marionette/client"
)

type apiFixture struct {
	mu                 sync.Mutex
	startFails         bool
	acquires, releases int
	actions            []map[string]any
}

func (f *apiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer personal-secret" {
		http.Error(w, `{"error":"invalid bearer"}`, 401)
		return
	}
	if r.URL.Path == "/api/devices" {
		_, _ = w.Write([]byte(`{"devices":[{"serial":"phone","model":"Pixel","state":"device"}]}`))
		return
	}
	if r.URL.Path != "/api/devices/phone/lease" || r.Method != "POST" {
		if r.Header.Get("X-Lease-Token") != "lease-secret" {
			http.Error(w, `{"error":"invalid lease"}`, 403)
			return
		}
	}
	switch r.URL.Path {
	case "/api/devices/phone/lease":
		if r.Method == "DELETE" {
			f.releases++
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.Method == "POST" {
			f.acquires++
		}
		_ = json.NewEncoder(w).Encode(client.Lease{Token: "lease-secret", Owner: "Member", ExpiresAt: time.Now().Add(time.Minute)})
	case "/api/devices/phone/start":
		if f.startFails {
			http.Error(w, `{"error":"driver unavailable"}`, 502)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	case "/api/devices/phone/actions":
		var action map[string]any
		_ = json.NewDecoder(r.Body).Decode(&action)
		f.actions = append(f.actions, action)
		if action["action"] == "text" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"focused field is not editable"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": action})
	case "/api/devices/phone/screenshot":
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png-fixture"))
	default:
		http.NotFound(w, r)
	}
}

func bridgeSession(t *testing.T, f *apiFixture) (context.Context, *mcp.ClientSession, *Bridge) {
	t.Helper()
	host := httptest.NewServer(f)
	t.Cleanup(host.Close)
	api, err := client.New(host.URL, "personal-secret")
	if err != nil {
		t.Fatal(err)
	}
	server, bridge := New(api)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	t.Cleanup(func() { _ = bridge.Close(ctx) })
	return ctx, session, bridge
}

func call(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, arguments any, wantError bool) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != wantError {
		t.Fatalf("%s IsError=%t, want %t: %+v", name, result.IsError, wantError, result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "personal-secret") || strings.Contains(string(encoded), "lease-secret") {
		t.Fatalf("%s exposed credentials", name)
	}
	return result
}

func TestToolsLeaseActionsAndImages(t *testing.T) {
	f := &apiFixture{}
	ctx, session, bridge := bridgeSession(t, f)
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 10 {
		t.Fatalf("got %d tools, want 10", len(tools.Tools))
	}
	device := map[string]any{"serial": "phone"}
	call(t, ctx, session, "list_devices", map[string]any{}, false)
	call(t, ctx, session, "tap", map[string]any{"serial": "phone", "x": 10, "y": 20}, true)
	call(t, ctx, session, "acquire_device", device, false)
	call(t, ctx, session, "acquire_device", device, true)
	call(t, ctx, session, "renew_device", device, false)
	call(t, ctx, session, "device_info", device, false)
	call(t, ctx, session, "tap", map[string]any{"serial": "phone", "x": 10, "y": 20}, false)
	call(t, ctx, session, "swipe", map[string]any{"serial": "phone", "x1": 10, "y1": 20, "x2": 30, "y2": 40}, false)
	call(t, ctx, session, "press_key", map[string]any{"serial": "phone", "key": "HOME"}, false)
	call(t, ctx, session, "press_key", map[string]any{"serial": "phone", "key": "unknown"}, true)
	failed := call(t, ctx, session, "type_text", map[string]any{"serial": "phone", "text": "한글🙂"}, true)
	if text, ok := failed.Content[0].(*mcp.TextContent); !ok || !strings.Contains(text.Text, "not editable") {
		t.Fatalf("driver error missing: %+v", failed)
	}
	image := call(t, ctx, session, "screenshot", device, false)
	png, ok := image.Content[0].(*mcp.ImageContent)
	if !ok || png.MIMEType != "image/png" || string(png.Data) != "png-fixture" {
		t.Fatalf("invalid MCP image: %+v", image)
	}
	call(t, ctx, session, "release_device", device, false)
	call(t, ctx, session, "screenshot", device, true)
	call(t, ctx, session, "acquire_device", device, false)
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquires != 2 || f.releases != 2 || len(f.actions) != 5 {
		t.Fatalf("acquires=%d releases=%d actions=%v", f.acquires, f.releases, f.actions)
	}
	if f.actions[2]["durationMs"] != float64(300) || f.actions[3]["keyCode"] != float64(3) || f.actions[4]["text"] != "한글🙂" {
		t.Fatalf("action translation failed: %v", f.actions)
	}
}

func TestFailedStartReleasesLease(t *testing.T) {
	f := &apiFixture{startFails: true}
	ctx, session, bridge := bridgeSession(t, f)
	call(t, ctx, session, "acquire_device", map[string]any{"serial": "phone"}, true)
	if len(bridge.leases) != 0 {
		t.Fatal("failed driver start kept lease")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquires != 1 || f.releases != 1 {
		t.Fatalf("acquires=%d releases=%d", f.acquires, f.releases)
	}
}
