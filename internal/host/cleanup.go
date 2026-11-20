package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"marionette/internal/identity"
)

const DefaultWipeTimeout = 3 * time.Minute

type releaseReceipt struct {
	tokenHash [32]byte
	subject   string
	leaseID   string
	wipe      bool
}

func respondReleased(w http.ResponseWriter, receipt releaseReceipt) {
	w.Header().Set("X-Marionette-Released-Lease-ID", receipt.leaseID)
	if receipt.wipe {
		writeJSON(w, 202, map[string]bool{"released": true, "cleaning": true})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Wipe bool `json:"wipe"`
	}
	if r.Body != nil {
		if err := readJSON(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
			fail(w, 400, "invalid release JSON")
			return
		}
	}
	wipe := body.Wipe || r.URL.Query().Get("wipe") == "1"
	token := r.Header.Get("X-Lease-Token")
	hash := sha256.Sum256([]byte(token))
	user, _ := identity.FromContext(r.Context())
	serial := r.PathValue("serial")
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous := s.slots[serial]; previous != nil && token != "" && previous.lastRelease.tokenHash == hash && previous.lastRelease.subject == user.Subject {
		respondReleased(w, previous.lastRelease)
		return
	}
	d := s.authorizedLocked(w, r, token)
	if d == nil {
		return
	}
	l := d.lease
	if wipe {
		if err := s.setWipePendingLocked(d, true); err != nil {
			slog.Error("could not record device cleanup", "serial", serial)
			fail(w, 500, "could not record device cleanup")
			return
		}
	}
	receipt := releaseReceipt{tokenHash: hash, subject: l.subject, leaseID: l.ID, wipe: wipe}
	d.lastRelease = receipt
	done := s.revokeLeaseLocked(d)
	if wipe {
		s.startWipeLocked(d, l.target, done)
	} else {
		s.waitCleanupLocked(r.Context(), done)
	}
	respondReleased(w, receipt)
}

func (s *Server) wipeStateLocked(d *slot) string {
	if d.wipeCancel != nil {
		return "cleaning"
	}
	if d.wipePending || s.cfg.Machine != nil && s.cfg.Machine.WipeRequired(d.device.Serial) {
		return "cleaning_failed"
	}
	return ""
}

func (s *Server) setWipePendingLocked(d *slot, pending bool) error {
	if s.cfg.Machine != nil {
		if err := s.cfg.Machine.SetWipeRequired(d.device.Serial, pending); err != nil {
			return err
		}
	}
	d.wipePending = pending
	return nil
}

func (s *Server) adminCleanup(w http.ResponseWriter, r *http.Request) {
	s.adminWipe(w, r, true)
}

func (s *Server) adminRestore(w http.ResponseWriter, r *http.Request) {
	s.adminWipe(w, r, false)
}

func (s *Server) adminWipe(w http.ResponseWriter, r *http.Request, retry bool) {
	if !identity.IsAdmin(r.Context()) {
		fail(w, 403, "administrator role required")
		return
	}
	serial := r.PathValue("serial")
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[serial]
	switch {
	case s.closed:
		fail(w, 503, "server shutting down")
	case d == nil:
		fail(w, 404, "unknown device")
	case s.wipeStateLocked(d) != "cleaning_failed" || d.lease != nil:
		fail(w, 409, "device has no failed cleanup")
	case retry:
		if !s.deviceOnlineLocked(d) || !s.allows(serial) {
			fail(w, 409, "device is not authorized and online")
			return
		}
		if s.quarantinedLocked(d) {
			fail(w, 409, "reconnect the device after resolving the lease input cleanup failure")
			return
		}
		s.startWipeLocked(d, d.device.target(), d.leaseCleanup)
		writeJSON(w, 202, map[string]bool{"cleaning": true})
	default:
		if err := s.setWipePendingLocked(d, false); err != nil {
			slog.Error("could not clear device cleanup record", "serial", serial)
			fail(w, 500, "could not clear device cleanup record")
			return
		}
		s.reconcileLocked(d)
		writeJSON(w, 200, map[string]bool{"restored": true})
	}
}

func (s *Server) startWipeLocked(d *slot, target adbTarget, leaseCleanup <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.WipeTimeout)
	d.wipeCancel = cancel
	s.cleanup.Add(1)
	go func() {
		defer s.cleanup.Done()
		defer cancel()
		result := errors.New("cleanup worker interrupted")
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := ctx.Err(); err != nil {
				result = err
			}
			if result == nil && (s.closed || d.device.target() != target || d.device.State != "device") {
				result = errors.New("device transport changed")
			}
			if result == nil && s.setWipePendingLocked(d, false) != nil {
				result = errors.New("could not clear device cleanup record")
			}
			d.wipeCancel = nil
			if result != nil {
				slog.Warn("device cleanup failed", "serial", target.serial, "reason", result)
			} else {
				s.reconcileLocked(d)
			}
		}()
		defer logPanic("device cleanup")
		if leaseCleanup != nil {
			select {
			case <-leaseCleanup:
			case <-ctx.Done():
				result = ctx.Err()
				return
			}
		}
		if err := s.drainForWipe(ctx, d); err != nil {
			result = err
			return
		}
		result = s.wipeAndroid(ctx, target)
	}()
}

func (s *Server) drainForWipe(ctx context.Context, d *slot) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		blocked := s.quarantinedLocked(d)
		drained := d.deviceCalls == 0 && d.adbStreams == 0 && d.directPeers == 0 && d.leaseCleanup == nil && d.work == nil
		s.mu.Unlock()
		if blocked {
			return errors.New("lease input or rotation cleanup failed")
		}
		if drained {
			return ctx.Err()
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Server) wipeAndroid(ctx context.Context, target adbTarget) error {
	adbShell := func(command string) ([]byte, error) {
		return s.cfg.ADB.Run(ctx, target.args("shell", command)...)
	}
	output, err := adbShell("pm list packages -3 --user 0")
	if err != nil {
		return errors.New("could not list third-party packages")
	}
	for line := range strings.Lines(string(output)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, found := strings.CutPrefix(line, "package:")
		if !found || !packageNamePattern.MatchString(name) {
			return errors.New("unexpected third-party package listing")
		}
		if slices.Contains(s.cfg.WipeKeepPackages, name) {
			continue
		}
		output, err := adbShell("pm uninstall --user 0 " + shellQuote(name))
		if err != nil || strings.TrimSpace(string(output)) != "Success" {
			return errors.New("could not uninstall a third-party package")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	seconds := max(1, int(time.Until(deadline)/time.Second))
	// Bound the remote file deletion too, in case the local ADB client exits
	// without the remote shell seeing the disconnect immediately.
	if _, err := adbShell(fmt.Sprintf("timeout -k 1 %d sh -c %s", seconds, shellQuote(wipeStorageScript()))); err != nil {
		return errors.New("could not empty shared storage")
	}
	if _, err := adbShell("input keyevent KEYCODE_HOME"); err != nil {
		return errors.New("could not return to the home screen")
	}
	return nil
}

func wipeStorageScript() string {
	folders := make([]string, 0, len(protectedFolders)-1)
	for name := range protectedFolders {
		if name != "Android" {
			folders = append(folders, shellQuote(name))
		}
	}
	slices.Sort(folders)
	// Glob inside the device shell so paths are never parsed or interpolated
	// on the host. Do not traverse symlinks even at a standard folder name.
	return `set -e
root=` + shellQuote(storageRoot) + `
[ -d "$root" ] && [ -r "$root" ] && [ -x "$root" ] || exit 1
for entry in "$root"/* "$root"/.[!.]* "$root"/..?*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  case "${entry##*/}" in
    Android) ;;
    ` + strings.Join(folders, "|") + `)
      if [ ! -L "$entry" ] && [ -d "$entry" ]; then
        [ -r "$entry" ] && [ -x "$entry" ] || exit 1
        for child in "$entry"/* "$entry"/.[!.]* "$entry"/..?*; do
          rm -rf -- "$child"
        done
      else
        rm -rf -- "$entry"
      fi ;;
    *) rm -rf -- "$entry" ;;
  esac
done`
}
