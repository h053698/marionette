package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type inputFrame struct {
	id, action, phase string
	fields            map[string]json.RawMessage
}

// Coalesce only adjacent movement samples. Down/up, keys and text are barriers:
// replacing a move never reorders it across one of those events.
type inputQueue struct {
	mu     sync.Mutex
	frames []inputFrame
	wake   chan struct{}
}

func (q *inputQueue) push(frame inputFrame) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.frames)
	if n > 0 && (frame.action == "touch" || frame.action == "hover") && frame.phase == "move" && q.frames[n-1].phase == "move" && frame.action == q.frames[n-1].action {
		q.frames[n-1] = frame
	} else {
		if n == 64 {
			return false
		}
		q.frames = append(q.frames, frame)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

func (q *inputQueue) pop(ctx context.Context) (inputFrame, bool) {
	for {
		if ctx.Err() != nil {
			return inputFrame{}, false
		}
		q.mu.Lock()
		if len(q.frames) > 0 {
			frame := q.frames[0]
			q.frames[0] = inputFrame{}
			q.frames = q.frames[1:]
			q.mu.Unlock()
			return frame, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return inputFrame{}, false
		case <-q.wake:
		}
	}
}

type nativeInput struct {
	conn   net.Conn
	reader *bufio.Reader
}

type controlSession struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	closed bool
	inputs []net.Conn
}

func (c *controlSession) add(conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		conn.Close()
		return false
	}
	c.inputs = append(c.inputs, conn)
	return true
}

func (c *controlSession) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for _, conn := range c.inputs {
		conn.Close()
	}
	c.ws.Close()
}

func (r *Runner) input(ctx context.Context, channel byte) (*nativeInput, error) {
	conn, err := r.connect(ctx, channel)
	if err != nil {
		return nil, err
	}
	return &nativeInput{conn: conn, reader: bufio.NewReaderSize(conn, 64<<10)}, nil
}

func (in *nativeInput) execute(frame inputFrame) (json.RawMessage, error) {
	data, err := json.Marshal(frame.fields)
	if err != nil {
		return nil, err
	}
	_ = in.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = in.conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	line, err := in.reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if !json.Valid(line) {
		return nil, errors.New("invalid native input response")
	}
	return append(json.RawMessage(nil), line...), nil
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{CheckOrigin: s.sameOrigin, HandshakeTimeout: 5 * time.Second}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(64 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if ws.ReadJSON(&auth) != nil || auth.Type != "auth" || len(auth.Token) > 256 {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "lease authentication required"), time.Now().Add(time.Second))
		return
	}
	s.mu.Lock()
	d, status, message := s.authorizeLocked(r, auth.Token)
	if d == nil {
		s.mu.Unlock()
		_ = ws.WriteJSON(map[string]any{"type": "error", "status": status, "error": message})
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "device access ended"), time.Now().Add(time.Second))
		return
	}
	if d.controls >= 2 || d.runner == nil || !d.runner.alive() {
		s.mu.Unlock()
		_ = ws.WriteJSON(map[string]any{"type": "error", "status": 409, "error": "device input is not available"})
		return
	}
	d.controls++
	runner, l := d.runner, d.lease
	session := &controlSession{ws: ws}
	l.controls[session] = struct{}{}
	l.inputRunners[runner] = struct{}{}
	s.mu.Unlock()
	defer func() {
		session.close()
		s.mu.Lock()
		delete(l.controls, session)
		d.controls--
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopLease := context.AfterFunc(l.ctx, cancel)
	defer stopLease()
	// Motion and reliable input have separate native sockets; neither shares the
	// process/tree/package query semaphore or waits for an HTTP round trip.
	startup, stopStartup := context.WithTimeout(ctx, 5*time.Second)
	defer stopStartup()
	hover, err := runner.input(startup, 3)
	if err != nil || !session.add(hover.conn) {
		return
	}
	commands, err := runner.input(startup, 4)
	if err != nil || !session.add(commands.conn) {
		return
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		defer logPanic("watch control connection")
		code, reason := websocket.ClosePolicyViolation, "device access ended"
		select {
		case <-l.done:
		case <-runner.done:
			code, reason = websocket.CloseInternalServerErr, "device input restarted"
		case <-r.Context().Done():
		case <-finished:
			return
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		cancel()
		session.close()
	}()
	responses := make(chan json.RawMessage, 64)
	queues := []*inputQueue{{wake: make(chan struct{}, 1)}, {wake: make(chan struct{}, 1)}}
	for i, input := range []*nativeInput{hover, commands} {
		go func(q *inputQueue, input *nativeInput) {
			defer logPanic("forward realtime input")
			defer cancel()
			defer session.close()
			for {
				frame, ok := q.pop(ctx)
				if !ok {
					return
				}
				s.mu.Lock()
				valid, _, _ := s.authorizeLocked(r, auth.Token)
				s.mu.Unlock()
				if valid != d || ctx.Err() != nil || l.ctx.Err() != nil {
					cancel()
					ws.Close()
					return
				}
				response, err := input.execute(frame)
				if err != nil {
					cancel()
					ws.Close()
					return
				}
				select {
				case responses <- response:
				case <-ctx.Done():
					return
				default:
					cancel()
					ws.Close()
					return
				}
			}
		}(queues[i], input)
	}
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		defer logPanic("write realtime input responses")
		defer session.close()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-responses:
				_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if ws.WriteMessage(websocket.TextMessage, data) != nil {
					cancel()
					ws.Close()
					return
				}
			case <-ticker.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					cancel()
					ws.Close()
					return
				}
			}
		}
	}()
	// Only this goroutine reads. The writer above exclusively owns JSON output.
	ready, _ := json.Marshal(map[string]any{"type": "ready"})
	responses <- ready
	_ = ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	for ctx.Err() == nil {
		var fields map[string]json.RawMessage
		if ws.ReadJSON(&fields) != nil {
			break
		}
		var frame inputFrame
		frame.fields = fields
		_ = json.Unmarshal(fields["id"], &frame.id)
		_ = json.Unmarshal(fields["action"], &frame.action)
		_ = json.Unmarshal(fields["phase"], &frame.phase)
		if frame.id == "" || len(frame.id) > 64 {
			break
		}
		switch frame.action {
		case "touch", "tap", "swipe", "hover", "key", "text":
			fields["leaseEpoch"] = leaseEpoch(l.epoch)
		default:
			cancel()
			ws.Close()
			continue
		}
		queue := queues[1]
		if frame.action == "hover" {
			queue = queues[0]
		}
		if !queue.push(frame) {
			break
		}
	}
	cancel()
	session.close()
	<-writeDone
}
