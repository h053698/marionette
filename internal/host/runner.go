package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

func randomID() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

type viewer struct {
	packets chan []byte
	waiting bool
}
type Runner struct {
	adb                   ADB
	target                adbTarget
	socket, port, pidfile string
	process               Process
	control, video        net.Conn
	done                  chan struct{}
	once                  sync.Once
	command               chan struct{}
	keyframes             chan struct{}
	pendingMu             sync.Mutex
	pending               map[string]chan json.RawMessage
	mu                    sync.Mutex
	viewers               map[*viewer]struct{}
	metadata, config      []byte
}

func startRunner(ctx context.Context, adb ADB, serial, jar string) (r *Runner, err error) {
	return startRunnerProfile(ctx, adb, adbTarget{serial: serial}, jar, "interactive")
}

func startRunnerProfile(ctx context.Context, adb ADB, target adbTarget, jar, profile string) (r *Runner, err error) {
	if profile != "preview" && profile != "interactive" {
		return nil, errors.New("invalid video profile")
	}
	r = &Runner{adb: adb, target: target, socket: "marionette_" + randomID(), done: make(chan struct{}), command: make(chan struct{}, 1), keyframes: make(chan struct{}, 1), pending: make(map[string]chan json.RawMessage), viewers: make(map[*viewer]struct{})}
	r.pidfile = "/data/local/tmp/" + r.socket + ".pid"
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if _, err = adb.Run(ctx, target.args("push", jar, "/data/local/tmp/marionette-device.jar")...); err != nil {
		return r, err
	}
	var out []byte
	out, err = adb.Run(ctx, target.args("forward", "tcp:0", "localabstract:"+r.socket)...)
	if err != nil {
		return r, err
	}
	p, e := strconv.Atoi(strings.TrimSpace(string(out)))
	if e != nil || p < 1 || p > 65535 {
		return r, errors.New("adb returned invalid forwarded port")
	}
	r.port = strconv.Itoa(p)
	script := "echo $$ > " + shellQuote(r.pidfile) + "; CLASSPATH=/data/local/tmp/marionette-device.jar exec app_process / dev.marionette.Main --socket " + shellQuote(r.socket)
	if profile == "preview" {
		script += " --max-size 480 --max-fps 8 --bit-rate 600000"
	}
	// The lifetime belongs to the runner, not to the HTTP start request.
	r.process, err = adb.Start(context.Background(), target.args("shell", script)...)
	if err != nil {
		return r, err
	}
	// adb can accept TCP before the abstract socket exists. Require a video
	// packet before considering startup successful; this also starts capture.
	for {
		r.video, err = r.connect(ctx, 1)
		if err != nil {
			return r, err
		}
		deadline := time.Now().Add(10 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = r.video.SetReadDeadline(deadline)
		var first []byte
		connection := r.video
		stopCancel := context.AfterFunc(ctx, func() { connection.Close() })
		first, err = ReadPacket(r.video)
		stopCancel()
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		if err == nil {
			_ = r.video.SetReadDeadline(time.Time{})
			switch first[0] {
			case 1:
				r.metadata = first
			case 2:
				r.config = first
			case 4:
				return r, errors.New("device capture: " + string(first[5:]))
			}
			break
		}
		r.video.Close()
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	r.control, err = r.connect(ctx, 2)
	if err != nil {
		return r, err
	}
	go func() { defer logPanic("read driver control"); r.readControl() }()
	go func() { defer logPanic("driver keyframes"); r.keyframeLoop() }()
	go func() { defer logPanic("read driver video"); r.readVideo() }()
	go func() {
		defer logPanic("watch driver process")
		select {
		case <-r.process.Done():
			r.Close()
		case <-r.done:
		}
	}()
	if profile == "interactive" {
		if err = r.warmup(ctx); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Capture readiness does not imply that accessibility has attached and supplied
// its first active root. Keep this cold-start work outside the first UI query.
func (r *Runner) warmup(ctx context.Context) error {
	raw, err := r.Action(ctx, map[string]json.RawMessage{"action": json.RawMessage(`"warmup"`)})
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &response)
	}
	if err == nil && !response.OK {
		if response.Error == "" {
			response.Error = "device did not confirm readiness"
		}
		err = errors.New(response.Error)
	}
	if err != nil {
		return fmt.Errorf("controller warmup failed: %w", err)
	}
	return nil
}
func (r *Runner) connect(ctx context.Context, kind byte) (net.Conn, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", r.port))
		if err == nil {
			_ = c.SetWriteDeadline(time.Now().Add(time.Second))
			_, err = c.Write([]byte{kind})
			_ = c.SetWriteDeadline(time.Time{})
			if err == nil {
				return c, nil
			}
			c.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.process.Done():
			return nil, errors.New("device process exited during startup")
		case <-ticker.C:
		}
	}
}
func (r *Runner) Close() {
	r.once.Do(func() {
		close(r.done)
		if r.control != nil {
			r.control.Close()
		}
		if r.video != nil {
			r.video.Close()
		}
		if r.process != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			// Check the unique socket in cmdline before killing the recorded PID, guarding
			// against PID reuse. All interpolated paths/names are generated by this host.
			script := "p=$(cat " + shellQuote(r.pidfile) + " 2>/dev/null); case \"$p\" in ''|*[!0-9]*) ;; *) if tr '\\000' ' ' < /proc/$p/cmdline 2>/dev/null | grep -F " + shellQuote(r.socket) + " >/dev/null; then kill \"$p\"; fi ;; esac; rm -f " + shellQuote(r.pidfile)
			_, _ = r.adb.Run(ctx, r.target.args("shell", script)...)
			cancel()
			_ = r.process.Stop()
		}
		if r.port != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = r.adb.Run(ctx, r.target.args("forward", "--remove", "tcp:"+r.port)...)
		}
	})
}
func (r *Runner) Action(ctx context.Context, fields map[string]json.RawMessage) (json.RawMessage, error) {
	return r.action(ctx, fields, nil)
}

func (r *Runner) action(ctx context.Context, fields map[string]json.RawMessage, l *lease) (reply json.RawMessage, err error) {
	started := time.Now()
	action, phase := controlActionName(fields), "queue"
	limit := 10 * time.Second
	if action == "warmup" {
		limit = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	defer func() {
		if err != nil {
			cause := "transport"
			if errors.Is(err, context.Canceled) {
				cause = "canceled"
			} else if errors.Is(err, context.DeadlineExceeded) {
				cause = "deadline"
			}
			// Do not log fields, selector values, text, request IDs or remote error text.
			slog.Warn("device control failed", "action", action, "phase", phase,
				"elapsedMs", time.Since(started).Milliseconds(), "cause", cause)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case r.command <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, errors.New("runner stopped")
	}
	defer func() { <-r.command }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := randomID()
	request := make(map[string]json.RawMessage, len(fields)+1)
	for k, v := range fields {
		request[k] = v
	}
	request["id"], _ = json.Marshal(id)
	delete(request, "leaseEpoch")
	if l != nil && leaseInputAction(action) {
		request["leaseEpoch"] = leaseEpoch(l.epoch)
	}
	b, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(b)+1 > MaxControl {
		return nil, errors.New("control request too large")
	}
	response := make(chan json.RawMessage, 1)
	r.pendingMu.Lock()
	r.pending[id] = response
	r.pendingMu.Unlock()
	defer func() { r.pendingMu.Lock(); delete(r.pending, id); r.pendingMu.Unlock() }()
	phase = "write"
	deadline, _ := ctx.Deadline()
	_ = r.control.SetWriteDeadline(deadline)
	if _, err = r.control.Write(append(b, '\n')); err != nil {
		go func() { defer logPanic("close driver after write failure"); r.Close() }()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	phase = "response"
	select {
	case b := <-response:
		if err := ctx.Err(); err != nil {
			if l == nil || l.ctx.Err() == nil {
				go func() { defer logPanic("close canceled driver"); r.Close() }()
			}
			return nil, err
		}
		return b, nil
	case <-ctx.Done():
		// Lease revocation sends a reset over a separate connection. For an
		// individual request cancellation, preserve the existing driver stop.
		if l == nil || l.ctx.Err() == nil {
			go func() { defer logPanic("close canceled driver"); r.Close() }()
		}
		return nil, ctx.Err()
	case <-r.done:
		return nil, errors.New("runner disconnected")
	}
}

func controlActionName(fields map[string]json.RawMessage) string {
	var action string
	_ = json.Unmarshal(fields["action"], &action)
	switch action {
	case "warmup", "uiTree", "findElement", "click", "text", "tap", "hover", "touch", "swipe", "key", "launchApp", "rotate", "apps", "appIcon", "processes", "info", "keyframe", "videoProfile":
		return action
	default:
		return "other"
	}
}

func leaseEpoch(epoch uint64) json.RawMessage {
	return strconv.AppendUint(nil, epoch, 10)
}

func leaseInputAction(action string) bool {
	switch action {
	case "touch", "tap", "swipe", "hover", "key", "text", "click", "rotate", "launchApp":
		return true
	default:
		return false
	}
}

func (r *Runner) resetInput(ctx context.Context, epoch uint64) error {
	if !r.alive() {
		return errors.New("driver stopped before input cleanup")
	}
	in, err := r.input(ctx, 5)
	if err != nil {
		return err
	}
	defer in.conn.Close()
	stop := context.AfterFunc(ctx, func() { in.conn.Close() })
	defer stop()
	response, err := in.execute(inputFrame{fields: map[string]json.RawMessage{
		"id": json.RawMessage(`"reset"`), "action": json.RawMessage(`"resetInput"`), "leaseEpoch": leaseEpoch(epoch),
	}})
	if err != nil {
		return err
	}
	var result struct {
		ID string
		OK bool
	}
	if json.Unmarshal(response, &result) != nil || result.ID != "reset" || !result.OK {
		return errors.New("driver did not acknowledge input cleanup")
	}
	return nil
}

func (r *Runner) readControl() {
	defer r.Close()
	s := bufio.NewScanner(r.control)
	s.Buffer(make([]byte, 4096), MaxControl)
	for s.Scan() {
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(s.Bytes(), &v) != nil {
			slog.Warn("device control protocol failed", "reason", "invalid-json")
			return
		}
		if v.ID == "" {
			slog.Warn("device control protocol failed", "reason", "missing-request-id")
			return
		}
		r.pendingMu.Lock()
		ch := r.pending[v.ID]
		r.pendingMu.Unlock()
		if ch != nil {
			select {
			case ch <- append(json.RawMessage(nil), s.Bytes()...):
			default:
			}
		}
	}
	if s.Err() != nil {
		select {
		case <-r.done:
		default:
			slog.Warn("device control protocol failed", "reason", "read-failed")
		}
	}
}
func (r *Runner) subscribe() *viewer {
	v := &viewer{packets: make(chan []byte, 16), waiting: true}
	r.mu.Lock()
	if r.metadata != nil {
		v.packets <- r.metadata
	}
	if r.config != nil {
		v.packets <- r.config
	}
	r.viewers[v] = struct{}{}
	r.mu.Unlock()
	r.requestKeyframe()
	return v
}

func (r *Runner) requestKeyframe() {
	select {
	case r.keyframes <- struct{}{}:
	default:
	}
}

// One worker and one queued request bound IDR work across all slow viewers.
func (r *Runner) keyframeLoop() {
	for {
		select {
		case <-r.done:
			return
		case <-r.keyframes:
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			_, _ = r.Action(ctx, map[string]json.RawMessage{"action": json.RawMessage(`"keyframe"`)})
			cancel()
		}
	}
}

func drainPackets(v *viewer) {
	for {
		select {
		case <-v.packets:
		default:
			return
		}
	}
}
func (r *Runner) unsubscribe(v *viewer) { r.mu.Lock(); delete(r.viewers, v); r.mu.Unlock() }
func (r *Runner) viewerCount() int      { r.mu.Lock(); defer r.mu.Unlock(); return len(r.viewers) }
func (r *Runner) readVideo() {
	defer r.Close()
	for {
		b, err := ReadPacket(r.video)
		if err != nil {
			return
		}
		r.broadcast(b)
	}
}

func (r *Runner) broadcast(b []byte) {
	needKeyframe := false
	r.mu.Lock()
	if b[0] == 1 {
		r.metadata = b
		r.config = nil
	}
	if b[0] == 2 {
		r.config = b
	}
	for v := range r.viewers {
		if b[0] == 1 || b[0] == 2 {
			drainPackets(v)
			v.waiting = true
			if r.metadata != nil {
				v.packets <- r.metadata
			}
			if r.config != nil {
				v.packets <- r.config
			}
			continue
		}
		if b[0] == 3 && (r.metadata == nil || r.config == nil || (v.waiting && !keyframe(b))) {
			continue
		}
		select {
		case v.packets <- b:
			if keyframe(b) {
				v.waiting = false
			}
		default:
			// Flush dependent frames and restore decoder context before the next IDR.
			drainPackets(v)
			v.waiting = true
			if r.metadata != nil {
				v.packets <- r.metadata
			}
			if r.config != nil {
				v.packets <- r.config
			}
			if keyframe(b) {
				v.packets <- b
				v.waiting = false
			} else if b[0] == 3 {
				needKeyframe = true
			} else if b[0] == 4 {
				v.packets <- b
			}
		}
	}
	r.mu.Unlock()
	if needKeyframe {
		r.requestKeyframe()
	}
}
func (r *Runner) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}
