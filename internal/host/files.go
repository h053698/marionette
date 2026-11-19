package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Only the emulated shared storage is reachable from the browser. App-private
// data stays out of reach, and every path is cleaned before it reaches a shell.
const (
	storageRoot              = "/sdcard"
	DefaultMaxUploadBytes    = 1 << 30
	DefaultMaxDownloadBytes  = 512 << 20
	DefaultMaxLeaseTransfers = 2
	DefaultMaxAgentTransfers = 8
	DefaultMaxStagingBytes   = 2 << 30
	maxListed                = 5000
	maxListingBytes          = 2 << 20
)

var packageNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)
var installFailure = regexp.MustCompile(`\[([A-Z][A-Z0-9_]+)\]`)

// Standard top-level folders are never deleted from the file manager.
var protectedFolders = map[string]bool{
	"Alarms": true, "Android": true, "Audiobooks": true, "DCIM": true, "Documents": true, "Download": true,
	"Movies": true, "Music": true, "Notifications": true, "Pictures": true, "Podcasts": true, "Recordings": true, "Ringtones": true,
}

type streamer interface {
	Stream(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
}

// Stream runs adb with piped input and output so large files never sit in memory.
func (a ExecADB) Stream(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	command := a.command(ctx, args...)
	command.Stdin, command.Stdout = stdin, stdout
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("adb: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := 2048 - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
func (l *limitedBuffer) String() string { return string(l.b) }

var errADBOutputLimit = errors.New("ADB output limit exceeded")

type cappedOutput struct {
	buf       bytes.Buffer
	remaining int
	overflow  bool
}

func (o *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > o.remaining {
		n, _ := o.buf.Write(p[:o.remaining])
		o.remaining = 0
		o.overflow = true
		return n, errADBOutputLimit
	}
	n, err := o.buf.Write(p)
	o.remaining -= n
	return n, err
}

func (s *Server) runCapped(ctx context.Context, limit int, args ...string) ([]byte, error) {
	if stream, ok := s.cfg.ADB.(streamer); ok {
		out := &cappedOutput{remaining: limit}
		err := stream.Stream(ctx, nil, out, args...)
		if out.overflow {
			// adb can exit on the closed pipe before os/exec reports the copy error.
			err = errADBOutputLimit
		}
		return out.buf.Bytes(), err
	}
	// In-memory ADB substitutes need not emulate process streaming.
	out, err := s.cfg.ADB.Run(ctx, args...)
	if len(out) > limit {
		return out[:limit], errADBOutputLimit
	}
	return out, err
}

func cleanStoragePath(raw string) (string, error) {
	if raw == "" {
		return storageRoot, nil
	}
	if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n") || !strings.HasPrefix(raw, "/") {
		return "", errors.New("invalid path")
	}
	cleaned := path.Clean(raw)
	if cleaned != storageRoot && !strings.HasPrefix(cleaned, storageRoot+"/") {
		return "", errors.New("only shared storage (/sdcard) is accessible")
	}
	return cleaned, nil
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

type deviceCall struct {
	target adbTarget
	lease  *lease
	slot   *slot
	ctx    context.Context
	cancel context.CancelFunc
}

// beginDevice authorizes the lease holder and ties the work to the lease so a
// revoked lease cancels a running transfer.
func (s *Server) beginDevice(w http.ResponseWriter, r *http.Request, timeout time.Duration) (*deviceCall, bool) {
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		s.mu.Unlock()
		return nil, false
	}
	l := d.lease
	d.deviceCalls++
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	stop := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel()
	}
	return &deviceCall{target: l.target, lease: l, slot: d, ctx: ctx, cancel: func() {
		stop()
		cancel()
		s.mu.Lock()
		d.deviceCalls--
		s.mu.Unlock()
	}}, true
}

// Reserve capacity before reading a body or starting a stream. APKs reserve
// their entire declared size until the staging file has been removed.
func (s *Server) reserveTransfer(w http.ResponseWriter, call *deviceCall, bytes int64) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || call.lease.ctx.Err() != nil {
		fail(w, 403, "device access ended")
		return nil, false
	}
	if bytes > s.cfg.MaxStagingBytes {
		fail(w, 413, "APK exceeds the agent staging capacity")
		return nil, false
	}
	if call.lease.transfers >= s.cfg.MaxLeaseTransfers || s.transfers >= s.cfg.MaxAgentTransfers || bytes > s.cfg.MaxStagingBytes-s.stagedBytes {
		fail(w, 429, "transfer capacity exhausted")
		return nil, false
	}
	call.lease.transfers++
	s.transfers++
	s.stagedBytes += bytes
	s.cleanup.Add(1)
	return func() {
		s.mu.Lock()
		call.lease.transfers--
		s.transfers--
		s.stagedBytes -= bytes
		s.mu.Unlock()
		s.cleanup.Done()
	}, true
}

func transferBody(w http.ResponseWriter, r *http.Request, ctx context.Context) (io.Reader, func()) {
	controller := http.NewResponseController(w)
	stop := context.AfterFunc(ctx, func() {
		// net/http's request body Close may wait for an outstanding Read.
		if controller.SetReadDeadline(time.Now()) != nil {
			_ = r.Body.Close()
		}
	})
	body := http.MaxBytesReader(w, r.Body, r.ContentLength)
	return &contextReader{ctx: ctx, reader: body}, func() { stop() }
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 20*time.Second)
	if !ok {
		return
	}
	defer call.cancel()
	dir, err := cleanStoragePath(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	script := "cd " + shellQuote(dir) + " || exit 3; find . -maxdepth 1 -mindepth 1 -exec stat -L -c '%F|%s|%Y|%n' {} + 2>/dev/null; true"
	out, err := s.runCapped(call.ctx, maxListingBytes, call.target.args("shell", script)...)
	truncated := errors.Is(err, errADBOutputLimit)
	if err != nil && !truncated {
		fail(w, 404, "folder not found or not readable")
		return
	}
	type entry struct {
		Name     string `json:"name"`
		Dir      bool   `json:"dir"`
		Size     int64  `json:"size"`
		Modified int64  `json:"modified"`
	}
	entries := make([]entry, 0)
	if truncated {
		// The final entry may have been cut off by the byte cap.
		out = out[:bytes.LastIndexByte(out, '\n')+1]
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "|", 4)
		if len(parts) != 4 {
			continue
		}
		dirEntry := parts[0] == "directory"
		if !dirEntry && !strings.HasPrefix(parts[0], "regular") {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		modified, _ := strconv.ParseInt(parts[2], 10, 64)
		name := strings.TrimPrefix(parts[3], "./")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if len(entries) >= maxListed {
			truncated = true
			break
		}
		entries = append(entries, entry{Name: name, Dir: dirEntry, Size: size, Modified: modified})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := ""
	if dir != storageRoot {
		parent = path.Dir(dir)
	}
	writeJSON(w, 200, map[string]any{"path": dir, "parent": parent, "entries": entries, "truncated": truncated})
}

func (s *Server) fileDownload(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 15*time.Minute)
	if !ok {
		return
	}
	defer call.cancel()
	file, err := cleanStoragePath(r.URL.Query().Get("path"))
	if err != nil || file == storageRoot {
		fail(w, 400, "invalid file path")
		return
	}
	stream, ok := s.cfg.ADB.(streamer)
	if !ok {
		fail(w, 501, "file transfer unavailable on this agent")
		return
	}
	release, ok := s.reserveTransfer(w, call, 0)
	if !ok {
		return
	}
	defer release()
	out, err := s.cfg.ADB.Run(call.ctx, call.target.args("shell", "stat -L -c '%F|%s' "+shellQuote(file)+" 2>/dev/null")...)
	kind, sizeText, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	size, parseErr := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || parseErr != nil || !strings.HasPrefix(kind, "regular") {
		fail(w, 404, "file not found")
		return
	}
	if size > s.cfg.MaxDownloadBytes {
		fail(w, 413, "file is too large to download in the browser")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+strings.ReplaceAll(url.PathEscape(path.Base(file)), "+", "%2B"))
	stop := context.AfterFunc(call.ctx, func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Now()) })
	defer stop()
	_ = stream.Stream(call.ctx, nil, w, call.target.args("exec-out", "cat "+shellQuote(file))...)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 30*time.Minute)
	if !ok {
		return
	}
	defer call.cancel()
	target, err := cleanStoragePath(r.URL.Query().Get("path"))
	if err != nil || target == storageRoot {
		fail(w, 400, "invalid file path")
		return
	}
	if r.ContentLength < 0 {
		fail(w, 411, "Content-Length is required")
		return
	}
	if r.ContentLength > s.cfg.MaxUploadBytes {
		fail(w, 413, fmt.Sprintf("file is larger than the upload limit of %d bytes", s.cfg.MaxUploadBytes))
		return
	}
	stream, ok := s.cfg.ADB.(streamer)
	if !ok {
		fail(w, 501, "file transfer unavailable on this agent")
		return
	}
	release, ok := s.reserveTransfer(w, call, 0)
	if !ok {
		return
	}
	defer release()
	reader, stopBody := transferBody(w, r, call.ctx)
	defer stopBody()
	// mktemp creates the file exclusively in the destination's directory.
	dir := path.Dir(target)
	prefix := path.Join(dir, ".marionette-part-"+randomID()+"-")
	script := "[ -d " + shellQuote(dir) + " ] || exit 3; umask 077; mktemp " + shellQuote(prefix+"XXXXXX")
	setup, stopSetup := context.WithTimeout(context.WithoutCancel(call.ctx), 5*time.Second)
	created, err := s.cfg.ADB.Run(setup, call.target.args("shell", script)...)
	stopSetup()
	if err != nil {
		switch exitCode(err) {
		case 3:
			fail(w, 404, "destination folder not found")
		default:
			fail(w, 502, "device is not reachable")
		}
		return
	}
	partial := strings.TrimRight(string(created), "\r\n")
	if suffix, ok := strings.CutPrefix(partial, prefix); !ok || len(suffix) != 6 || strings.ContainsAny(suffix, "/\x00\r\n") {
		fail(w, 502, "could not prepare the upload on the device")
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = s.cfg.ADB.Run(cleanup, call.target.args("shell", "rm -f -- "+shellQuote(partial))...)
	}()
	body := &countingReader{r: reader}
	err = stream.Stream(call.ctx, body, io.Discard, call.target.args("exec-in", "cat > "+shellQuote(partial))...)
	if err != nil || call.ctx.Err() != nil || body.n != r.ContentLength {
		fail(w, 502, "upload did not complete")
		return
	}
	// Some Android mv implementations check before renaming. Serialize commits
	// on this device so concurrent Marionette uploads can't race that check.
	commit, cancelCommit := context.WithTimeout(call.ctx, 10*time.Second)
	defer cancelCommit()
	select {
	case call.slot.fileCommit <- struct{}{}:
		defer func() { <-call.slot.fileCommit }()
	case <-commit.Done():
		fail(w, 502, "could not save the file on the device")
		return
	}
	move := "mv -nT -- " + shellQuote(partial) + " " + shellQuote(target) + "; " +
		"[ ! -e " + shellQuote(partial) + " ] && exit 0; " +
		"if [ -e " + shellQuote(target) + " ] || [ -L " + shellQuote(target) + " ]; then exit 4; fi; exit 5"
	if r.URL.Query().Get("overwrite") == "1" {
		move = "mv -fT -- " + shellQuote(partial) + " " + shellQuote(target)
	}
	// exec-in has no remote exit status. Check the staged size before publishing.
	move = "[ \"$(stat -c %s -- " + shellQuote(partial) + ")\" = " + strconv.FormatInt(body.n, 10) + " ] || exit 5; " + move
	if _, err := s.cfg.ADB.Run(commit, call.target.args("shell", move)...); err != nil {
		if exitCode(err) == 4 {
			fail(w, 409, "a file with this name already exists")
			return
		}
		fail(w, 502, "could not save the file on the device")
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true, "path": target, "size": body.n})
}

func (s *Server) fileMkdir(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 20*time.Second)
	if !ok {
		return
	}
	defer call.cancel()
	dir, err := cleanStoragePath(r.URL.Query().Get("path"))
	if err != nil || dir == storageRoot {
		fail(w, 400, "invalid folder path")
		return
	}
	script := "[ -d " + shellQuote(path.Dir(dir)) + " ] || exit 3; [ ! -e " + shellQuote(dir) + " ] || exit 4; mkdir " + shellQuote(dir)
	if _, err := s.cfg.ADB.Run(call.ctx, call.target.args("shell", script)...); err != nil {
		switch exitCode(err) {
		case 3:
			fail(w, 404, "parent folder not found")
		case 4:
			fail(w, 409, "a file or folder with this name already exists")
		default:
			fail(w, 502, "could not create the folder")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"ok": true, "path": dir})
}

func (s *Server) fileDelete(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 60*time.Second)
	if !ok {
		return
	}
	defer call.cancel()
	target, err := cleanStoragePath(r.URL.Query().Get("path"))
	if err != nil || target == storageRoot {
		fail(w, 400, "invalid path")
		return
	}
	if path.Dir(target) == storageRoot && protectedFolders[path.Base(target)] {
		fail(w, 403, "standard folders cannot be deleted")
		return
	}
	if _, err := s.cfg.ADB.Run(call.ctx, call.target.args("shell", "[ -e "+shellQuote(target)+" ] || exit 3; rm -rf -- "+shellQuote(target))...); err != nil {
		if exitCode(err) == 3 {
			fail(w, 404, "file not found")
			return
		}
		fail(w, 502, "could not delete")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) appInstall(w http.ResponseWriter, r *http.Request) {
	call, ok := s.beginDevice(w, r, 10*time.Minute)
	if !ok {
		return
	}
	defer call.cancel()
	if r.ContentLength < 0 {
		fail(w, 411, "Content-Length is required")
		return
	}
	if r.ContentLength > s.cfg.MaxUploadBytes {
		fail(w, 413, fmt.Sprintf("APK is larger than the upload limit of %d bytes", s.cfg.MaxUploadBytes))
		return
	}
	release, ok := s.reserveTransfer(w, call, r.ContentLength)
	if !ok {
		return
	}
	defer release()
	file, err := os.CreateTemp("", "marionette-install-*.apk")
	if err != nil {
		fail(w, 500, "could not prepare the APK")
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	body, stopBody := transferBody(w, r, call.ctx)
	defer stopBody()
	received, err := io.Copy(file, body)
	if err != nil || call.ctx.Err() != nil || received != r.ContentLength {
		fail(w, 400, "upload did not complete")
		return
	}
	magic := make([]byte, 4)
	if _, err := file.ReadAt(magic, 0); err != nil || string(magic) != "PK\x03\x04" {
		fail(w, 400, "this is not an APK file")
		return
	}
	if _, err := s.cfg.ADB.Run(call.ctx, call.target.args("install", "-r", file.Name())...); err != nil {
		code := "INSTALL_FAILED"
		if match := installFailure.FindStringSubmatch(err.Error()); match != nil {
			code = match[1]
		}
		fail(w, 422, "install failed: "+code)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) appUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PackageName string `json:"packageName"`
	}
	if err := readJSON(w, r, &body); err != nil || len(body.PackageName) > 255 || !packageNamePattern.MatchString(body.PackageName) {
		fail(w, 400, "invalid package name")
		return
	}
	call, ok := s.beginDevice(w, r, 60*time.Second)
	if !ok {
		return
	}
	defer call.cancel()
	if _, err := s.cfg.ADB.Run(call.ctx, call.target.args("uninstall", body.PackageName)...); err != nil {
		fail(w, 422, "uninstall failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
