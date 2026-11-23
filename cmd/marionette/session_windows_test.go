package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSessionWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := secureSessionFile(file); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	// Windows can retain inheritance bookkeeping flags on a protected DACL.
	// Normalize those flags while preserving protection and every access entry.
	if err := sd.SetControl(windows.SE_DACL_AUTO_INHERITED|windows.SE_DACL_AUTO_INHERIT_REQ, 0); err != nil {
		t.Fatal(err)
	}
	if got, want := sd.String(), expected.String(); got != want {
		t.Fatalf("session ACL = %s, want %s", got, want)
	}
}
