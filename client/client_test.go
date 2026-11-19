package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCredentialsStayAtConfiguredOrigin(t *testing.T) {
	var forwarded atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer personal-secret" {
			t.Error("API request did not authenticate")
		}
		http.Redirect(w, r, destination.URL+"/api/devices", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	api, err := New(origin.URL, "personal-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Devices(context.Background())
	var failure *APIError
	if !errors.As(err, &failure) || failure.Status != http.StatusTemporaryRedirect {
		t.Fatalf("redirect should fail: %v", err)
	}
	if forwarded.Load() {
		t.Fatal("API client followed a redirect outside its configured origin")
	}
}

func TestRejectUnencryptedRemoteAPI(t *testing.T) {
	for _, origin := range []string{"http://devices.example.com", "http://192.168.1.10:8080", "https://user:secret@devices.example.com", "https://devices.example.com/api", "https://devices.example.com?token=secret"} {
		if _, err := New(origin, "personal-secret"); err == nil {
			t.Errorf("accepted invalid API origin %s", origin)
		}
	}
	for _, origin := range []string{"https://devices.example.com", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := New(origin, "personal-secret"); err != nil {
			t.Errorf("rejected valid API origin %s: %v", origin, err)
		}
	}
}
