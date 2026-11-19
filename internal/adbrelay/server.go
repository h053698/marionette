// Package adbrelay presents a loopback adbd transport to the user's local ADB
// server. Each ADB OPEN is translated to one authenticated device service on the
// remote agent, preserving shell v2, sync and JDWP protocols.
package adbrelay

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Stream is one remote ADB service connection, carried over the backend
// WebSocket relay or a direct WebRTC data channel.
type Stream interface {
	Read() ([]byte, error)
	Write(ctx context.Context, data []byte) error
	Close() error
}

// Opener starts one device service on the assigned device.
type Opener func(ctx context.Context, service string) (Stream, error)

// WebSocket adapts a relay connection from client.OpenADB.
func WebSocket(ws *websocket.Conn) Stream { return wsStream{ws} }

type wsStream struct{ ws *websocket.Conn }

func (s wsStream) Read() ([]byte, error) {
	kind, data, err := s.ws.ReadMessage()
	if err == nil && kind != websocket.BinaryMessage {
		err = errors.New("unexpected ADB relay message")
	}
	return data, err
}
func (s wsStream) Write(_ context.Context, data []byte) error {
	_ = s.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return s.ws.WriteMessage(websocket.BinaryMessage, data)
}
func (s wsStream) Close() error { return s.ws.Close() }

const maxPayload = 256 << 10

var connect = command("CNXN")
var open = command("OPEN")
var okay = command("OKAY")
var write = command("WRTE")
var closeCommand = command("CLSE")

func command(value string) uint32 { return binary.LittleEndian.Uint32([]byte(value)) }

type Server struct {
	open     Opener
	banner   string
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New binds IPv4 loopback only. Port zero chooses an unused local port.
func New(ctx context.Context, open Opener, model, features string, port int) (*Server, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return nil, err
	}
	allowed := make([]string, 0)
	// Transport flow control uses one OKAY per WRTE; delayed_ack is excluded.
	for _, feature := range strings.Split(features, ",") {
		switch feature {
		case "shell_v2", "cmd", "stat_v2", "ls_v2", "fixed_push_mkdir", "apex", "abb", "abb_exec", "fixed_push_symlink_timestamp", "sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd", "track_app":
			allowed = append(allowed, feature)
		}
	}
	model = strings.Map(func(r rune) rune {
		if r <= 32 || strings.ContainsRune(";:\x00", r) {
			return '_'
		}
		return r
	}, model)
	ctx, cancel := context.WithCancel(ctx)
	s := &Server{open: open, listener: listener, ctx: ctx, cancel: cancel, banner: "device::ro.product.name=Marionette;ro.product.model=" + model + ";ro.product.device=marionette_remote;features=" + strings.Join(allowed, ",") + ";\x00"}
	return s, nil
}
func (s *Server) Address() string { return s.listener.Addr().String() }
func (s *Server) Close()          { s.cancel(); s.listener.Close(); s.wg.Wait() }
func (s *Server) Run() error {
	stop := context.AfterFunc(s.ctx, func() { s.listener.Close() })
	defer stop()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.serve(conn) }()
	}
}

type packet struct {
	command, arg0, arg1 uint32
	data                []byte
}

func readPacket(reader io.Reader) (packet, error) {
	var p packet
	var header [24]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return p, err
	}
	p.command = binary.LittleEndian.Uint32(header[0:4])
	p.arg0 = binary.LittleEndian.Uint32(header[4:8])
	p.arg1 = binary.LittleEndian.Uint32(header[8:12])
	length := binary.LittleEndian.Uint32(header[12:16])
	if length > maxPayload || binary.LittleEndian.Uint32(header[20:24]) != p.command^0xffffffff {
		return p, errors.New("invalid ADB packet")
	}
	p.data = make([]byte, length)
	if _, err := io.ReadFull(reader, p.data); err != nil {
		return p, err
	}
	checksum := binary.LittleEndian.Uint32(header[16:20])
	var actual uint32
	for _, b := range p.data {
		actual += uint32(b)
	}
	if checksum != 0 && checksum != actual {
		return p, errors.New("invalid ADB checksum")
	}
	return p, nil
}

type transport struct {
	server  *Server
	conn    net.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	writeMu sync.Mutex
	mu      sync.Mutex
	streams map[uint32]*stream
	next    uint32
	limit   int
}
type stream struct {
	parent        *transport
	local, remote uint32
	ack           chan struct{}
	done          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	once          sync.Once
	mu            sync.Mutex
	conn          Stream
	opened        bool
}

func (s *Server) serve(conn net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	t := &transport{server: s, conn: conn, ctx: ctx, cancel: cancel, streams: make(map[uint32]*stream), limit: maxPayload}
	defer func() {
		t.mu.Lock()
		streams := make([]*stream, 0, len(t.streams))
		for _, v := range t.streams {
			streams = append(streams, v)
		}
		t.mu.Unlock()
		for _, v := range streams {
			v.finish(false)
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := readPacket(conn)
	if err != nil || first.command != connect || first.arg0 < 0x01000000 || first.arg1 < 4096 {
		return
	}
	if int(first.arg1) < t.limit {
		t.limit = int(first.arg1)
	}
	version := uint32(0x01000001)
	if first.arg0 < version {
		version = first.arg0
	}
	if t.send(packet{command: connect, arg0: version, arg1: maxPayload, data: []byte(s.banner)}) != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	for {
		p, err := readPacket(conn)
		if err != nil {
			return
		}
		if p.command == open {
			service := strings.TrimSuffix(string(p.data), "\x00")
			t.mu.Lock()
			if len(t.streams) >= 64 || p.arg0 == 0 || p.arg1 != 0 || service == "" || len(service) > 65535 || strings.ContainsRune(service, 0) {
				t.mu.Unlock()
				_ = t.send(packet{command: closeCommand, arg1: p.arg0})
				continue
			}
			t.next++
			local := t.next
			streamCtx, stop := context.WithCancel(ctx)
			v := &stream{parent: t, local: local, remote: p.arg0, ack: make(chan struct{}, 1), done: make(chan struct{}), ctx: streamCtx, cancel: stop}
			t.streams[local] = v
			t.mu.Unlock()
			go v.run(service)
			continue
		}
		t.mu.Lock()
		v := t.streams[p.arg1]
		t.mu.Unlock()
		if v == nil || v.remote != p.arg0 {
			continue
		}
		switch p.command {
		case okay:
			select {
			case v.ack <- struct{}{}:
			default:
			}
		case closeCommand:
			v.finish(true)
		case write:
			v.mu.Lock()
			conn := v.conn
			v.mu.Unlock()
			if conn == nil {
				v.finish(true)
				continue
			}
			if conn.Write(v.ctx, p.data) != nil {
				v.finish(true)
				continue
			}
			if t.send(packet{command: okay, arg0: v.local, arg1: v.remote}) != nil {
				return
			}
		default:
			return
		}
	}
}

func (t *transport) send(p packet) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	var header [24]byte
	binary.LittleEndian.PutUint32(header[0:4], p.command)
	binary.LittleEndian.PutUint32(header[4:8], p.arg0)
	binary.LittleEndian.PutUint32(header[8:12], p.arg1)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(p.data)))
	var sum uint32
	for _, b := range p.data {
		sum += uint32(b)
	}
	binary.LittleEndian.PutUint32(header[16:20], sum)
	binary.LittleEndian.PutUint32(header[20:24], p.command^0xffffffff)
	_ = t.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := io.Copy(t.conn, io.MultiReader(strings.NewReader(string(header[:])), strings.NewReader(string(p.data))))
	if err != nil {
		t.cancel()
	}
	return err
}
func (v *stream) finish(reply bool) {
	v.once.Do(func() {
		v.cancel()
		close(v.done)
		v.mu.Lock()
		if v.conn != nil {
			v.conn.Close()
		}
		opened := v.opened
		v.mu.Unlock()
		v.parent.mu.Lock()
		delete(v.parent.streams, v.local)
		v.parent.mu.Unlock()
		if reply {
			local := uint32(0)
			if opened {
				local = v.local
			}
			// A rejected OPEN has no established peer ID. Some ADB servers ignore
			// a nonzero sender ID here and leave the service request waiting.
			_ = v.parent.send(packet{command: closeCommand, arg0: local, arg1: v.remote})
		}
	})
}
func (v *stream) run(service string) {
	defer v.finish(true)
	conn, err := v.parent.server.open(v.ctx, service)
	if err != nil {
		return
	}
	v.mu.Lock()
	if v.ctx.Err() != nil {
		v.mu.Unlock()
		conn.Close()
		return
	}
	v.conn = conn
	err = v.parent.send(packet{command: okay, arg0: v.local, arg1: v.remote})
	v.opened = err == nil
	v.mu.Unlock()
	if err != nil {
		return
	}
	for {
		data, err := conn.Read()
		if err != nil {
			return
		}
		for len(data) > 0 {
			n := min(len(data), v.parent.limit)
			if v.parent.send(packet{command: write, arg0: v.local, arg1: v.remote, data: data[:n]}) != nil {
				return
			}
			data = data[n:]
			select {
			case <-v.ack:
			case <-v.done:
				return
			case <-v.ctx.Done():
				return
			}
		}
	}
}
