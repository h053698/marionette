// Package direct carries ADB service streams over WebRTC data channels so the
// user's PC and the agent exchange device traffic without the backend relay.
// The backend only forwards the lease-authorized offer and answer.
package direct

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

const (
	// Messages stay well below the SCTP message limit every peer accepts.
	chunkSize   = 16 << 10
	maxMessage  = 64 << 10
	highWater   = 1 << 20
	lowWater    = 256 << 10
	sendTimeout = 15 * time.Second
	// DefaultSTUN is used when no STUN list is configured.
	DefaultSTUN = "stun:stun.l.google.com:19302"
	// MaxOffer bounds the signaling payload accepted from a client.
	MaxOffer = 64 << 10
)

// ICEServers parses a comma-separated STUN/TURN URL list. "none" gathers host
// candidates only, which is enough when both PCs share a network.
func ICEServers(raw string) []webrtc.ICEServer {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultSTUN
	}
	if raw == "none" {
		return nil
	}
	var urls []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			urls = append(urls, value)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: urls}}
}

// NewAPI returns a WebRTC API whose data channels are detached into streams.
func NewAPI() *webrtc.API {
	var settings webrtc.SettingEngine
	settings.DetachDataChannels()
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

// Gather waits for ICE candidate gathering so the description can be sent in
// one signaling round trip. Unreachable STUN servers end gathering early.
func Gather(ctx context.Context, pc *webrtc.PeerConnection) (string, error) {
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case <-webrtc.GatheringCompletePromise(pc):
	case <-timer.C:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	description := pc.LocalDescription()
	if description == nil {
		return "", errors.New("no local WebRTC description")
	}
	return description.SDP, nil
}

// Channel is one ordered, reliable data channel carrying a single ADB service.
type Channel struct {
	dc   *webrtc.DataChannel
	raw  datachannel.ReadWriteCloserDeadliner
	low  chan struct{}
	buf  []byte
	once sync.Once
}

// Open detaches dc. It must be called from the channel's OnOpen callback.
func Open(dc *webrtc.DataChannel) (*Channel, error) {
	raw, err := dc.DetachWithDeadline()
	if err != nil {
		return nil, err
	}
	c := &Channel{dc: dc, raw: raw, low: make(chan struct{}, 1), buf: make([]byte, maxMessage)}
	dc.SetBufferedAmountLowThreshold(lowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case c.low <- struct{}{}:
		default:
		}
	})
	return c, nil
}

// Write sends p in bounded chunks and waits while too much data is queued.
func (c *Channel) Write(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), chunkSize)
		for c.dc.BufferedAmount() > highWater {
			timer := time.NewTimer(sendTimeout)
			select {
			case <-c.low:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				return errors.New("direct ADB send timed out")
			}
		}
		if _, err := c.raw.Write(p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Read returns the next message. The slice is valid until the next Read.
// A zero deadline waits indefinitely.
func (c *Channel) Read(deadline time.Time) ([]byte, error) {
	_ = c.raw.SetReadDeadline(deadline)
	n, err := c.raw.Read(c.buf)
	if err != nil {
		return nil, err
	}
	return c.buf[:n], nil
}

func (c *Channel) Close() error {
	c.once.Do(func() {
		_ = c.raw.Close()
		_ = c.dc.Close()
	})
	return nil
}
