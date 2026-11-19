// Package mcpbridge adapts the REST API to a local stdio MCP server.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"marionette/client"
)

type Bridge struct {
	api    *client.Client
	mu     sync.Mutex // Order actions and lease changes, including process cleanup.
	leases map[string]client.Lease
}

type deviceInput struct {
	Serial string `json:"serial" jsonschema:"Device serial from list_devices"`
}
type releaseInput struct {
	Serial string `json:"serial" jsonschema:"Device serial from list_devices"`
	Wipe   bool   `json:"wipe,omitempty" jsonschema:"Set true to uninstall third-party Android apps (except configured keep packages) and clear files from shared storage when returning the device. System account logins may remain. Defaults to false."`
}
type leaseInput struct {
	Serial     string   `json:"serial,omitempty" jsonschema:"Device serial from list_devices; omit to select by platform, tags and model"`
	TTLSeconds int      `json:"ttlSeconds,omitempty" jsonschema:"Lease duration 30 to 3600 seconds; default 300. Renew explicitly before expiry."`
	Platform   string   `json:"platform,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Model      string   `json:"model,omitempty"`
}
type infoInput struct {
	Serial    string           `json:"serial"`
	Apps      bool             `json:"apps,omitempty" jsonschema:"Return launchable Android apps with labels and package names instead of geometry"`
	Processes bool             `json:"processes,omitempty" jsonschema:"Return running Android app and system processes with PID, UID, state and resident memory instead of geometry"`
	Format    string           `json:"format,omitempty" jsonschema:"Set json or xml to read the accessibility UI tree instead of display geometry"`
	Selector  *client.Selector `json:"selector,omitempty" jsonschema:"Find an accessible element by exact resourceId, text, description or className instead of reading geometry"`
	WaitMS    int              `json:"waitMs,omitempty" jsonschema:"Wait for the selected element, 0 to 8000 milliseconds"`
}
type tapInput struct {
	Serial      string           `json:"serial"`
	PackageName string           `json:"packageName,omitempty" jsonschema:"Launch an installed Android app by package name instead of tapping; discover packages with device_info apps=true"`
	X           *int             `json:"x,omitempty" jsonschema:"Horizontal pixel in the full resolution screenshot; omit when using selector"`
	Y           *int             `json:"y,omitempty" jsonschema:"Vertical pixel in the full resolution screenshot; omit when using selector"`
	Selector    *client.Selector `json:"selector,omitempty"`
	WaitMS      int              `json:"waitMs,omitempty"`
}
type swipeInput struct {
	Serial     string `json:"serial"`
	X1         int    `json:"x1"`
	Y1         int    `json:"y1"`
	X2         int    `json:"x2"`
	Y2         int    `json:"y2"`
	DurationMS int    `json:"durationMs,omitempty" jsonschema:"Swipe duration in milliseconds; default 300"`
}
type keyInput struct {
	Serial string `json:"serial"`
	Key    string `json:"key" jsonschema:"Android key name (HOME BACK ENTER APP_SWITCH DEL TAB ESCAPE POWER VOLUME_UP VOLUME_DOWN DPAD_UP DPAD_DOWN DPAD_LEFT DPAD_RIGHT) or numeric keycode"`
}
type textInput struct {
	Serial   string           `json:"serial"`
	Text     string           `json:"text" jsonschema:"Complete replacement text for the focused accessible editable field; supports Unicode"`
	Selector *client.Selector `json:"selector,omitempty" jsonschema:"Target a field directly by resourceId or other exact selector; no coordinate tap is needed"`
	WaitMS   int              `json:"waitMs,omitempty"`
}

func New(api *client.Client) (*mcp.Server, *Bridge) {
	b := &Bridge{api: api, leases: make(map[string]client.Lease)}
	s := mcp.NewServer(&mcp.Implementation{Name: "marionette", Version: "1.0.0"}, &mcp.ServerOptions{
		Instructions: "Control devices through Marionette without knowing their host. Each account may hold one device across browser, CLI and MCP sessions. list_devices shows only this account's assigned device; an empty list does not mean the pool is empty. Start with acquire_device: specify the assigned serial to resume it, or omit serial and select by platform/tags/model for a new assignment. device_info returns geometry by default; provide format=json or xml for a UI tree, selector plus waitMs to find an element, or apps=true to list launchable Android apps. tap accepts coordinates, a selector, or packageName to launch an app; type_text accepts a selector to target an editable field directly. Coordinates use full physical screenshot pixels. Leases default to 300 seconds; renew_device explicitly. release_device when finished. Never include credentials in tool arguments.",
	})
	add(s, "list_devices", "List this account's assigned device and lease expiry. Use acquire_device without a serial when this list is empty.", true, func(ctx context.Context, _ struct{}) (any, error) {
		devices, err := api.Devices(ctx)
		return map[string]any{"devices": devices}, err
	})
	add(s, "acquire_device", "Exclusively lease a device and start its control driver. Other users' leases cannot be taken over. Lease credentials stay inside this MCP process.", false, b.acquire)
	add(s, "renew_device", "Extend a device lease owned by this MCP process (30–3600 seconds, default 300).", false, b.renew)
	add(s, "release_device", "Release a device acquired by this MCP process so another user can use it.", false, b.release)
	add(s, "device_info", "Read geometry/capabilities, launchable Android apps (apps=true), running app/system processes (processes=true), an accessibility UI tree (format=json|xml), or a matching element (selector, optional waitMs).", true, func(ctx context.Context, in infoInput) (any, error) {
		if in.Processes {
			if in.Apps || in.Selector != nil || in.Format != "" {
				return nil, errors.New("provide processes, apps, selector, or format separately")
			}
			return b.action(ctx, in.Serial, map[string]any{"action": "processes"})
		}
		if in.Apps {
			if in.Selector != nil || in.Format != "" {
				return nil, errors.New("provide apps, selector, or format separately")
			}
			return b.action(ctx, in.Serial, map[string]any{"action": "apps"})
		}
		if in.Selector != nil {
			return b.action(ctx, in.Serial, map[string]any{"action": "findElement", "selector": in.Selector, "waitMs": in.WaitMS})
		}
		if in.Format != "" {
			return b.action(ctx, in.Serial, map[string]any{"action": "uiTree", "format": in.Format})
		}
		return b.action(ctx, in.Serial, map[string]any{"action": "info"})
	})
	add(s, "tap", "Launch an Android app with packageName, click an element using selector and optional waitMs, or tap at full resolution x/y. Supply exactly one targeting method.", false, func(ctx context.Context, in tapInput) (any, error) {
		if in.PackageName != "" {
			if in.Selector != nil || in.X != nil || in.Y != nil {
				return nil, errors.New("provide packageName, selector, or x/y separately")
			}
			return b.action(ctx, in.Serial, map[string]any{"action": "launchApp", "packageName": in.PackageName})
		}
		if in.Selector != nil {
			if in.X != nil || in.Y != nil {
				return nil, errors.New("provide selector or x/y, not both")
			}
			return b.action(ctx, in.Serial, map[string]any{"action": "click", "selector": in.Selector, "waitMs": in.WaitMS})
		}
		if in.X == nil || in.Y == nil {
			return nil, errors.New("provide both x and y, or a selector")
		}
		return b.action(ctx, in.Serial, map[string]any{"action": "tap", "x": *in.X, "y": *in.Y})
	})
	add(s, "swipe", "Swipe an acquired device between full resolution screenshot coordinates.", false, func(ctx context.Context, in swipeInput) (any, error) {
		if in.DurationMS == 0 {
			in.DurationMS = 300
		}
		return b.action(ctx, in.Serial, map[string]any{"action": "swipe", "x1": in.X1, "y1": in.Y1, "x2": in.X2, "y2": in.Y2, "durationMs": in.DurationMS})
	})
	add(s, "press_key", "Send an Android key such as HOME, BACK or ENTER to an acquired device.", false, func(ctx context.Context, in keyInput) (any, error) {
		code, err := keyCode(in.Key)
		if err != nil {
			return nil, err
		}
		return b.action(ctx, in.Serial, map[string]any{"action": "key", "keyCode": code})
	})
	add(s, "type_text", "Replace the focused editable field with complete text, including Korean. Tap the field first. This does not append; password or inaccessible fields may reject input.", false, func(ctx context.Context, in textInput) (any, error) {
		if in.Selector != nil {
			return b.action(ctx, in.Serial, map[string]any{"action": "text", "text": in.Text, "selector": in.Selector, "waitMs": in.WaitMS})
		}
		return b.action(ctx, in.Serial, map[string]any{"action": "text", "text": in.Text})
	})
	mcp.AddTool(s, &mcp.Tool{Name: "screenshot", Description: "Return a PNG image of an acquired device at physical resolution. Use its original pixel dimensions for taps and swipes.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deviceInput) (*mcp.CallToolResult, any, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			lease, err := b.owned(in.Serial)
			if err != nil {
				return nil, nil, err
			}
			png, err := api.Screenshot(ctx, in.Serial, lease.Token)
			if err != nil {
				b.forgetRevoked(in.Serial, err)
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil, nil
		})
	return s, b
}

func add[In any](s *mcp.Server, name, description string, readOnly bool, fn func(context.Context, In) (any, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			out, err := fn(ctx, in)
			return nil, out, err
		})
}

func leaseResult(serial string, lease client.Lease) any {
	return map[string]any{"serial": serial, "owner": lease.Owner, "expiresAt": lease.ExpiresAt}
}

func (b *Bridge) owned(serial string) (client.Lease, error) {
	lease, ok := b.leases[serial]
	if !ok || !time.Now().Before(lease.ExpiresAt) {
		delete(b.leases, serial)
		return client.Lease{}, errors.New("no active lease in this MCP process; call acquire_device first")
	}
	return lease, nil
}

// Revocation is final for this credential. Forget it so the next explicit
// acquire can succeed without waiting for the former expiry or restarting MCP.
func (b *Bridge) forgetRevoked(serial string, err error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403 || apiErr.Status == 404 || apiErr.Status == 410) {
		delete(b.leases, serial)
	}
}

func (b *Bridge) acquire(ctx context.Context, in leaseInput) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if in.Serial == "" {
		// One account holds one device. If this bridge already holds a live lease,
		// hand it back instead of failing, so an agent that lost track can resume.
		for serial, lease := range b.leases {
			if time.Now().Before(lease.ExpiresAt) {
				return map[string]any{"serial": serial, "owner": lease.Owner, "expiresAt": lease.ExpiresAt, "resumed": true}, nil
			}
		}
		assignment, err := b.api.Occupy(ctx, client.Selection{Platform: in.Platform, Tags: in.Tags, Model: in.Model}, in.TTLSeconds)
		if err != nil {
			return nil, err
		}
		b.leases[assignment.Assigned] = assignment.Lease
		return map[string]any{"serial": assignment.Assigned, "device": assignment.Device, "expiresAt": assignment.Lease.ExpiresAt}, nil
	}
	if _, err := b.owned(in.Serial); err == nil {
		return nil, errors.New("already acquired by this MCP process; use renew_device or release_device")
	}
	lease, err := b.api.Acquire(ctx, in.Serial, in.TTLSeconds)
	if err != nil {
		return nil, err
	}
	b.leases[in.Serial] = lease
	if err := b.api.Start(ctx, in.Serial, lease.Token); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if releaseErr := b.api.Release(cleanup, in.Serial, lease.Token); releaseErr != nil {
			return nil, fmt.Errorf("driver start failed: %w; cleanup failed: %v; call release_device or wait for expiry", err, releaseErr)
		}
		delete(b.leases, in.Serial)
		return nil, err
	}
	return leaseResult(in.Serial, lease), nil
}

func (b *Bridge) renew(ctx context.Context, in leaseInput) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease, err := b.owned(in.Serial)
	if err != nil {
		return nil, err
	}
	lease, err = b.api.Renew(ctx, in.Serial, lease.Token, in.TTLSeconds)
	if err == nil {
		b.leases[in.Serial] = lease
	} else {
		b.forgetRevoked(in.Serial, err)
	}
	return leaseResult(in.Serial, lease), err
}

func (b *Bridge) release(ctx context.Context, in releaseInput) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease, err := b.owned(in.Serial)
	if err != nil {
		return nil, err
	}
	if err := b.api.ReleaseWith(ctx, in.Serial, lease.Token, in.Wipe); err != nil {
		b.forgetRevoked(in.Serial, err)
		return nil, err
	}
	delete(b.leases, in.Serial)
	return map[string]bool{"released": true}, nil
}

func (b *Bridge) action(ctx context.Context, serial string, action any) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease, err := b.owned(serial)
	if err != nil {
		return nil, err
	}
	result, err := b.api.Action(ctx, serial, lease.Token, action)
	if err != nil {
		b.forgetRevoked(serial, err)
		return nil, err
	}
	if len(result) == 0 {
		return map[string]bool{"ok": true}, nil
	}
	var value any
	err = json.Unmarshal(result, &value)
	return value, err
}

// Close best-effort releases all leases. A killed/disconnected process is still
// bounded by the host's lease expiry; it cannot renew leases in the background.
func (b *Bridge) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var failures []error
	for serial, lease := range b.leases {
		if time.Now().Before(lease.ExpiresAt) {
			if err := b.api.Release(ctx, serial, lease.Token); err != nil {
				failures = append(failures, fmt.Errorf("release %s: %w", serial, err))
			}
		}
		delete(b.leases, serial)
	}
	return errors.Join(failures...)
}

func keyCode(key string) (int, error) {
	key = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(key)), "KEYCODE_")
	codes := map[string]int{"HOME": 3, "BACK": 4, "DPAD_UP": 19, "DPAD_DOWN": 20, "DPAD_LEFT": 21, "DPAD_RIGHT": 22, "DPAD_CENTER": 23, "VOLUME_UP": 24, "VOLUME_DOWN": 25, "POWER": 26, "TAB": 61, "ENTER": 66, "DEL": 67, "BACKSPACE": 67, "ESCAPE": 111, "APP_SWITCH": 187}
	if code, ok := codes[key]; ok {
		return code, nil
	}
	if code, err := strconv.Atoi(key); err == nil && code >= 0 && code <= 1000 {
		return code, nil
	}
	return 0, errors.New("unknown Android key; use HOME, BACK, ENTER or a numeric keycode")
}
