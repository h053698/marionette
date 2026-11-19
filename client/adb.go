package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type ADBHello struct {
	OK       bool   `json:"ok"`
	Features string `json:"features"`
	Error    string `json:"error"`
}

// OpenADB opens one device service, or "watch" for a lease-bound control stream.
// The caller owns Close and must keep a single reader and writer per connection.
func (c *Client) OpenADB(ctx context.Context, deviceID, lease, service string) (*websocket.Conn, ADBHello, error) {
	var hello ADBHello
	path, err := devicePath(deviceID)
	if err != nil {
		return nil, hello, err
	}
	origin := strings.Replace(c.baseURL, "https://", "wss://", 1)
	origin = strings.Replace(origin, "http://", "ws://", 1)
	header := http.Header{"Authorization": []string{"Bearer " + c.token}, "X-Lease-Token": []string{lease}}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, response, err := dialer.DialContext(ctx, origin+path+"/adb", header)
	if err != nil {
		if response != nil {
			response.Body.Close()
			return nil, hello, &APIError{Status: response.StatusCode, Message: "ADB connection rejected"}
		}
		return nil, hello, errors.New("ADB tunnel connection failed")
	}
	ws.SetReadLimit(1 << 20)
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = ws.WriteJSON(map[string]string{"service": service}); err == nil {
		err = ws.ReadJSON(&hello)
	}
	if err != nil || !hello.OK {
		ws.Close()
		if hello.Error != "" {
			return nil, hello, errors.New(hello.Error)
		}
		return nil, hello, errors.New("ADB service handshake failed")
	}
	_ = ws.SetReadDeadline(time.Time{})
	_ = ws.SetWriteDeadline(time.Time{})
	return ws, hello, nil
}

// DirectOffer forwards a WebRTC offer to the agent holding deviceID and
// returns its answer. The lease token authorizes the peer connection.
func (c *Client) DirectOffer(ctx context.Context, deviceID, lease, sdp string) (string, error) {
	path, err := devicePath(deviceID)
	if err != nil {
		return "", err
	}
	data, _, err := c.request(ctx, "POST", path+"/direct", lease, map[string]string{"sdp": sdp})
	if err != nil {
		return "", err
	}
	var answer struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || answer.SDP == "" {
		return "", errors.New("invalid WebRTC answer")
	}
	return answer.SDP, nil
}
