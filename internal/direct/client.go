package direct

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Hello is the agent's reply to a service request.
type Hello struct {
	OK       bool   `json:"ok"`
	Features string `json:"features"`
	Error    string `json:"error"`
}

// Peer is the CLI side of a direct connection to one agent.
type Peer struct {
	pc        *webrtc.PeerConnection
	mu        sync.Mutex
	bootstrap *webrtc.DataChannel
	done      chan struct{}
	once      sync.Once
}

// Connect sends an offer through signal and waits until the agent is
// reachable peer to peer. signal returns the agent's answer SDP.
func Connect(ctx context.Context, ice []webrtc.ICEServer, signal func(context.Context, string) (string, error)) (*Peer, error) {
	pc, err := NewAPI().NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return nil, err
	}
	p := &Peer{pc: pc, done: make(chan struct{})}
	connected := make(chan struct{})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			p.Close()
		}
	})
	// The offer needs one data channel to negotiate SCTP; it becomes the
	// first service stream.
	p.bootstrap, err = pc.CreateDataChannel("adb", nil)
	if err == nil {
		var offer webrtc.SessionDescription
		offer, err = pc.CreateOffer(nil)
		if err == nil {
			err = pc.SetLocalDescription(offer)
		}
	}
	var sdp, answer string
	if err == nil {
		sdp, err = Gather(ctx, pc)
	}
	if err == nil {
		answer, err = signal(ctx, sdp)
	}
	if err == nil {
		err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case <-connected:
		return p, nil
	case <-p.done:
		return nil, errors.New("direct connection failed")
	case <-timer.C:
		p.Close()
		return nil, errors.New("direct connection timed out")
	case <-ctx.Done():
		p.Close()
		return nil, ctx.Err()
	}
}

// Done closes when the peer connection ends.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) Close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.pc.Close()
	})
}

// Open requests one ADB service ("watch" or a device service) on a new
// ordered data channel and returns it after the agent accepts.
func (p *Peer) Open(ctx context.Context, service string) (*Channel, Hello, error) {
	var hello Hello
	p.mu.Lock()
	dc := p.bootstrap
	p.bootstrap = nil
	p.mu.Unlock()
	if dc == nil {
		var err error
		if dc, err = p.pc.CreateDataChannel("adb", nil); err != nil {
			return nil, hello, err
		}
	}
	ready := make(chan *Channel, 1)
	failed := make(chan struct{})
	attach := func() {
		ch, err := Open(dc)
		if err != nil {
			close(failed)
			return
		}
		ready <- ch
	}
	dc.OnOpen(attach) // runs immediately when the channel is already open
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var ch *Channel
	select {
	case ch = <-ready:
	case <-failed:
		return nil, hello, errors.New("direct ADB channel failed")
	case <-p.done:
		return nil, hello, errors.New("direct connection closed")
	case <-timer.C:
		_ = dc.Close()
		return nil, hello, errors.New("direct ADB channel timed out")
	case <-ctx.Done():
		_ = dc.Close()
		return nil, hello, ctx.Err()
	}
	request, _ := json.Marshal(map[string]string{"service": service})
	if err := ch.Write(ctx, request); err != nil {
		ch.Close()
		return nil, hello, err
	}
	data, err := ch.Read(time.Now().Add(10 * time.Second))
	if err == nil {
		err = json.Unmarshal(data, &hello)
	}
	if err != nil || !hello.OK {
		ch.Close()
		if hello.Error != "" {
			return nil, hello, errors.New(hello.Error)
		}
		return nil, hello, errors.New("ADB service handshake failed")
	}
	return ch, hello, nil
}
