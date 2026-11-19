package host

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type screenshotADB struct {
	*fakeADB
	image   []byte
	started chan struct{}
}

func (a *screenshotADB) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 5 && strings.Join(args[2:], " ") == "exec-out screencap -p" {
		if a.started != nil {
			close(a.started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return a.image, nil
	}
	return a.fakeADB.Run(ctx, args...)
}

func TestScreenshotLeaseAndPNG(t *testing.T) {
	var imageData bytes.Buffer
	_ = png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 3)))
	adb := &screenshotADB{fakeADB: &fakeADB{listing: "phone device model:Pixel"}, image: imageData.Bytes()}
	s := New(Config{ADB: adb})
	s.discover(context.Background())
	defer s.Close()
	if w := request(t, s, "GET", "/api/devices/phone/screenshot", "", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	token := claimToken(t, s)
	w := request(t, s, "GET", "/api/devices/phone/screenshot", token, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), imageData.Bytes()) {
		t.Fatalf("screenshot: %d %s", w.Code, w.Body.String())
	}
	adb.image = []byte("not a PNG")
	if w = request(t, s, "GET", "/api/devices/phone/screenshot", token, ""); w.Code != 502 {
		t.Fatal("invalid screenshot accepted")
	}
	adb.started = make(chan struct{})
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- request(t, s, "GET", "/api/devices/phone/screenshot", token, "") }()
	select {
	case <-adb.started:
	case <-time.After(time.Second):
		t.Fatal("capture did not start")
	}
	if w = request(t, s, "DELETE", "/api/devices/phone/lease", token, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	select {
	case w = <-done:
		if w.Code != 403 {
			t.Fatalf("capture survived revoke: %d", w.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("capture not canceled")
	}
}
