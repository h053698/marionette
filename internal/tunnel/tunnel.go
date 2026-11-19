// Package tunnel carries independent HTTP connections over an outbound agent
// WebSocket. Yamux supplies flow control so a slow video stream has its own queue.
package tunnel

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const Protocol = "marionette-agent-v1"

func Config() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.StreamOpenTimeout = 10 * time.Second
	cfg.StreamCloseTimeout = 5 * time.Second
	return cfg
}

// Conn adapts binary WebSocket messages to the ordered byte stream yamux needs.
// Yamux owns the single reader and writer; deadlines belong to its substreams.
type Conn struct {
	*websocket.Conn
	reader io.Reader
}

func New(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(1 << 20)
	return &Conn{Conn: ws}
}

func (c *Conn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, reader, err := c.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				return 0, errors.New("agent tunnel requires binary messages")
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	// Bound individual messages even when a caller writes a large HTTP body.
	written := 0
	for len(p) > 0 {
		size := min(len(p), 64<<10)
		if err := c.WriteMessage(websocket.BinaryMessage, p[:size]); err != nil {
			return written, err
		}
		written += size
		p = p[size:]
	}
	return written, nil
}

func (c *Conn) LocalAddr() net.Addr                { return c.UnderlyingConn().LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.UnderlyingConn().RemoteAddr() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.UnderlyingConn().SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.UnderlyingConn().SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.UnderlyingConn().SetWriteDeadline(t) }
