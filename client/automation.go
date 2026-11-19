package client

import (
	"context"
	"encoding/json"
	"errors"
)

// Attach consumes a browser handoff code and rotates the existing lease. Both
// browser session and this client's personal token must identify the same user.
func (c *Client) Attach(ctx context.Context, deviceID, code string) (Lease, error) {
	var lease Lease
	path, err := devicePath(deviceID)
	if err != nil {
		return lease, err
	}
	data, _, err := c.request(ctx, "POST", path+"/attach", "", map[string]string{"code": code})
	if err == nil {
		err = json.Unmarshal(data, &lease)
	}
	return lease, err
}

// Selection matches all supplied tags. Empty fields accept any available device.
type Selection struct {
	Platform string   `json:"platform,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Model    string   `json:"model,omitempty"`
	DeviceID string   `json:"deviceId,omitempty"`
	// Kind is "physical" or "virtual"; empty accepts either.
	Kind string `json:"kind,omitempty"`
}

type Assignment struct {
	Assigned string `json:"assigned"`
	Device   Device `json:"device"`
	Lease    Lease  `json:"lease"`
}

// ErrLeaseNotCurrent means the server no longer lists the saved lease as this
// account's assignment. Refreshing it must not acquire a replacement lease.
var ErrLeaseNotCurrent = errors.New("saved lease is no longer assigned to this account; explicitly occupy or attach a device again")

// RefreshAssignment reads the current account's assignment without acquiring or
// renewing a lease. The lease ID must still match; the saved token remains subject
// to server authorization on every subsequent device operation.
func (c *Client) RefreshAssignment(ctx context.Context, assignment Assignment) (Assignment, error) {
	if assignment.Assigned == "" || assignment.Lease.ID == "" || assignment.Lease.Token == "" {
		return Assignment{}, errors.New("invalid saved assignment")
	}
	devices, err := c.Devices(ctx)
	if err != nil {
		return Assignment{}, err
	}
	for _, device := range devices {
		if device.Serial != assignment.Assigned || !device.Mine || !device.Occupied || device.LeaseID != assignment.Lease.ID {
			continue
		}
		if device.ExpiresAt == nil || device.ExpiresAt.IsZero() {
			return Assignment{}, errors.New("invalid assignment expiry from server")
		}
		assignment.Device = device
		assignment.Lease.Owner = device.Owner
		assignment.Lease.ExpiresAt = *device.ExpiresAt
		return assignment, nil
	}
	return Assignment{}, ErrLeaseNotCurrent
}

type Selector struct {
	ResourceID  string `json:"resourceId,omitempty"`
	Text        string `json:"text,omitempty"`
	Description string `json:"description,omitempty"`
	ClassName   string `json:"className,omitempty"`
	Index       int    `json:"index,omitempty"`
}

// Occupy selects a free device across agents, acquires its exclusive lease and
// starts the persistent controller in one request. No ADB/host knowledge is needed.
func (c *Client) Occupy(ctx context.Context, selection Selection, ttlSeconds int) (Assignment, error) {
	data, _, err := c.request(ctx, "POST", "/api/device/occupy", "", struct {
		Selection
		TTLSeconds int `json:"ttlSeconds"`
	}{selection, ttlSeconds})
	var assignment Assignment
	if err == nil {
		err = json.Unmarshal(data, &assignment)
	}
	return assignment, err
}

func (c *Client) FindElement(ctx context.Context, serial, token string, selector Selector, waitMS int) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "findElement", "selector": selector, "waitMs": waitMS})
}

func (c *Client) Click(ctx context.Context, serial, token string, selector Selector, waitMS int) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "click", "selector": selector, "waitMs": waitMS})
}

func (c *Client) SetText(ctx context.Context, serial, token, text string, selector Selector, waitMS int) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "text", "selector": selector, "text": text, "waitMs": waitMS})
}

func (c *Client) UITree(ctx context.Context, serial, token, format string) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "uiTree", "format": format})
}

func (c *Client) Apps(ctx context.Context, serial, token string) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "apps"})
}

func (c *Client) AppIcon(ctx context.Context, serial, token, packageName string) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "appIcon", "packageName": packageName})
}

func (c *Client) Processes(ctx context.Context, serial, token string) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "processes"})
}

func (c *Client) LaunchApp(ctx context.Context, serial, token, packageName string) (json.RawMessage, error) {
	return c.Action(ctx, serial, token, map[string]any{"action": "launchApp", "packageName": packageName})
}
