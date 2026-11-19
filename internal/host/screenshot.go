package host

import (
	"bytes"
	"image/png"
	"net/http"
	"strconv"
	"time"
)

const maxScreenshotBytes = 20 << 20

// Capture the physical display so image coordinates match tap/swipe coordinates.
// No H.264 decoder or video subscriber is required for API/MCP screenshots.
func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 10*time.Second)
	if !ok {
		return
	}
	defer call.cancel()
	image, err := s.runCapped(call.ctx, maxScreenshotBytes, call.target.args("exec-out", "screencap", "-p")...)
	// Recheck after capture: a lease may have ended while adb was running.
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	valid := d != nil && d.lease == call.lease
	s.mu.Unlock()
	if !valid {
		return
	}
	if err != nil || call.ctx.Err() != nil {
		fail(w, 502, "device screenshot failed or timed out")
		return
	}
	writePNG(w, image)
}

func writePNG(w http.ResponseWriter, image []byte) {
	if _, err := png.DecodeConfig(bytes.NewReader(image)); err != nil || len(image) > maxScreenshotBytes {
		fail(w, 502, "device returned an invalid or oversized PNG")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	_, _ = w.Write(image)
}
