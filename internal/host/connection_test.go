package host

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"marionette/internal/identity"
	"marionette/internal/machine"
)

type reconnectADB struct {
	*fakeADB
	reconnects [][]string
}

func (a *reconnectADB) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 3 && args[2] == "reconnect" {
		a.reconnects = append(a.reconnects, args)
		return []byte("reconnecting locked [unauthorized]\n"), nil
	}
	return a.fakeADB.Run(ctx, args...)
}

func connectionRequest(s *Server, serial, action, role string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/admin/devices/"+serial+"/connection", strings.NewReader(`{"action":"`+action+`"}`))
	r.Host = "localhost"
	r = r.WithContext(identity.WithUser(r.Context(), identity.User{Subject: "tester", Role: role}))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestAdminPhysicalDeviceConnection(t *testing.T) {
	config := machine.Config{StateDir: t.TempDir(), ADB: "/unused/adb"}
	adb := &reconnectADB{fakeADB: &fakeADB{listing: "phone device transport_id:11\nlocked unauthorized transport_id:12\nusb-only unauthorized\nemulator-5554 device transport_id:13\n"}}
	config.DeviceAllow = []string{"physical:*", "emulator-5554"}
	start := func(t *testing.T) (*Server, func()) {
		t.Helper()
		m, err := machine.New(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		s := New(Config{ADB: adb, Machine: m})
		s.discover(context.Background())
		return s, func() { s.Close(); m.Close() }
	}
	s, stop := start(t)
	expect := func(serial, action, role string, code int, state string) {
		t.Helper()
		w := connectionRequest(s, serial, action, role)
		var result struct{ State string }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != code || result.State != state {
			t.Fatalf("%s %s: %d %s", serial, action, w.Code, w.Body)
		}
	}
	// Close the first instance even if a check fails before the restart.
	defer func() { stop() }()
	expect("phone", "disconnect", "user", 403, "")
	expect("emulator-5554", "disconnect", "admin", 409, "")
	expect("phone", "invalid", "admin", 400, "")
	expect("usb-only", "debug", "admin", 200, "debug_unavailable")
	expect("locked", "debug", "admin", 202, "debug_requested")
	if len(adb.reconnects) != 1 || !slices.Equal(adb.reconnects[0], []string{"-t", "12", "reconnect"}) {
		t.Fatalf("expected one targeted reconnect, got %v", adb.reconnects)
	}
	// The old transport can be retained for quarantine without targeting it
	// after the phone has disappeared from the current ADB listing.
	adb.mu.Lock()
	adb.listing = strings.Replace(adb.listing, "locked unauthorized transport_id:12\n", "locked usb\n", 1)
	adb.mu.Unlock()
	s.discover(context.Background())
	expect("locked", "debug", "admin", 200, "debug_unavailable")
	if len(adb.reconnects) != 1 {
		t.Fatalf("used a stale transport: %v", adb.reconnects)
	}
	lease := claimToken(t, s)
	expect("phone", "disconnect", "admin", 409, "")
	if w := request(t, s, "DELETE", "/api/devices/phone/lease", lease, ""); w.Code != 200 {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	expect("phone", "disconnect", "admin", 200, "disconnected")
	stop()
	stop = func() {}
	s, stop = start(t)
	s.mu.Lock()
	offline := !s.deviceOnlineLocked(s.devices["phone"]) && s.devices["phone"].online == nil
	s.mu.Unlock()
	if !offline {
		t.Fatal("disconnected phone became available on restart")
	}
	if w := request(t, s, "POST", "/api/devices/phone/lease", "", `{"owner":"tester"}`); w.Code != 409 {
		t.Fatalf("disconnected device lease: %d %s", w.Code, w.Body)
	}
	expect("phone", "reconnect", "admin", 200, "device")
	claimToken(t, s)
}
