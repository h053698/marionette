package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"marionette/client"
)

func TestSessionSaveReadAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".marionette", "session.json")
	state := session{URL: "https://example.com", Assignment: client.Assignment{
		Assigned: "device", Lease: client.Lease{ID: "lease", Token: "first-token"},
	}}
	for _, token := range []string{"first-token", "renewed-token"} {
		state.Assignment.Lease.Token = token
		if err := saveSession(path, state); err != nil {
			t.Fatal(err)
		}
		got, err := readSession(path, state.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignment.Lease.Token != token {
			t.Fatalf("saved token = %q, want %q", got.Assignment.Lease.Token, token)
		}
	}
	if _, err := readSession(path, "https://other.example.com"); err == nil {
		t.Fatal("accepted a session from another API origin")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readSession(path, state.URL); err == nil {
			t.Fatal("accepted a session readable by other users")
		}
	}
}

func TestFindADBInSDK(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANDROID_SDK_ROOT", "")
	root := t.TempDir()
	t.Setenv("ANDROID_HOME", root)
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	path := filepath.Join(root, "platform-tools", name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if got := findADB(); got != path {
		t.Fatalf("findADB() = %q, want %q", got, path)
	}
	if runtime.GOOS == "windows" {
		t.Setenv("ANDROID_HOME", "")
		local := t.TempDir()
		t.Setenv("LOCALAPPDATA", local)
		path = filepath.Join(local, "Android", "Sdk", "platform-tools", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0700); err != nil {
			t.Fatal(err)
		}
		if got := findADB(); got != path {
			t.Fatalf("findADB() = %q, want %q", got, path)
		}
	}
}
