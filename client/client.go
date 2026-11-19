// Package client calls the Marionette REST API with a personal API token.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type Device struct {
	Platform        string     `json:"platform"`
	Tags            []string   `json:"tags"`
	Serial          string     `json:"serial"`
	RawSerial       string     `json:"rawSerial"`
	AgentID         string     `json:"agentId"`
	Model           string     `json:"model"`
	State           string     `json:"state"`
	Occupied        bool       `json:"occupied"`
	Owner           string     `json:"owner"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	LeaseID         string     `json:"leaseId,omitempty"`
	Kind            string     `json:"kind"`
	Managed         bool       `json:"managed"`
	ControllerReady bool       `json:"controllerReady"`
	VideoProfile    string     `json:"videoProfile,omitempty"`
	Viewers         int        `json:"viewers"`
	Mine            bool       `json:"mine"`
	Name            string     `json:"name,omitempty"`
	AndroidVersion  string     `json:"androidVersion,omitempty"`
	SDK             int        `json:"sdk,omitempty"`
	Width           int        `json:"width,omitempty"`
	Height          int        `json:"height,omitempty"`
}

type Lease struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// APIError includes HTTP status for callers deciding whether to retry/reacquire.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("Marionette API (%d): %s", e.Status, e.Message) }

func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("MARIONETTE_URL must be an API origin such as https://devices.example.com")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("MARIONETTE_URL requires HTTPS except on loopback")
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("MARIONETTE_TOKEN is required; create one in the web console's API / MCP settings")
	}
	return &Client{baseURL: u.String(), token: token, http: &http.Client{
		Timeout:       40 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// NewWithTransport talks to an API served in the same process. The transport is
// responsible for authenticating the caller, so no bearer token is required.
func NewWithTransport(baseURL string, transport http.RoundTripper) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{
		Timeout:       40 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func devicePath(serial string) (string, error) {
	if serial == "" || serial == "." || serial == ".." || strings.ContainsAny(serial, "/\\\x00\r\n") {
		return "", errors.New("invalid device serial")
	}
	return "/api/devices/" + url.PathEscape(serial), nil
}

func (c *Client) request(ctx context.Context, method, path, lease string, body any) ([]byte, string, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if lease != "" {
		r.Header.Set("X-Lease-Token", lease)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	const maxResponse = 20 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxResponse {
		return nil, "", errors.New("API response exceeds 20 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(response.StatusCode)
		}
		return nil, "", &APIError{response.StatusCode, failure.Error}
	}
	return data, response.Header.Get("Content-Type"), nil
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	data, _, err := c.request(ctx, "GET", "/api/devices", "", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Devices []Device `json:"devices"`
	}
	err = json.Unmarshal(data, &result)
	return result.Devices, err
}

func (c *Client) lease(ctx context.Context, method, serial, token string, ttlSeconds int) (Lease, error) {
	path, err := devicePath(serial)
	if err != nil {
		return Lease{}, err
	}
	data, _, err := c.request(ctx, method, path+"/lease", token, map[string]int{"ttlSeconds": ttlSeconds})
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	err = json.Unmarshal(data, &lease)
	return lease, err
}

func (c *Client) Acquire(ctx context.Context, serial string, ttlSeconds int) (Lease, error) {
	return c.lease(ctx, "POST", serial, "", ttlSeconds)
}

func (c *Client) Renew(ctx context.Context, serial, token string, ttlSeconds int) (Lease, error) {
	return c.lease(ctx, "PUT", serial, token, ttlSeconds)
}

func (c *Client) Release(ctx context.Context, serial, token string) error {
	return c.ReleaseWith(ctx, serial, token, false)
}

// ReleaseWith optionally requests asynchronous Android cleanup on return.
func (c *Client) ReleaseWith(ctx context.Context, serial, token string, wipe bool) error {
	path, err := devicePath(serial)
	if err != nil {
		return err
	}
	var body any
	if wipe {
		body = map[string]bool{"wipe": true}
	}
	_, _, err = c.request(ctx, "DELETE", path+"/lease", token, body)
	return err
}

func (c *Client) Start(ctx context.Context, serial, token string) error {
	path, err := devicePath(serial)
	if err != nil {
		return err
	}
	_, _, err = c.request(ctx, "POST", path+"/start", token, nil)
	return err
}

// Action supports info, tap, swipe, key, text and keyframe. Text replaces the
// focused accessible field, including Unicode. Driver failures become errors.
func (c *Client) Action(ctx context.Context, serial, token string, action any) (json.RawMessage, error) {
	path, err := devicePath(serial)
	if err != nil {
		return nil, err
	}
	data, _, err := c.request(ctx, "POST", path+"/actions", token, action)
	if err != nil {
		return nil, err
	}
	var result struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		if result.Error == "" {
			result.Error = "device action failed"
		}
		return nil, errors.New(result.Error)
	}
	return result.Result, nil
}

func (c *Client) Screenshot(ctx context.Context, serial, token string) ([]byte, error) {
	path, err := devicePath(serial)
	if err != nil {
		return nil, err
	}
	data, contentType, err := c.request(ctx, "GET", path+"/screenshot", token, nil)
	if err == nil && contentType != "image/png" {
		err = errors.New("screenshot response is not image/png")
	}
	return data, err
}
