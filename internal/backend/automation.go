package backend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"marionette/client"
	"marionette/internal/identity"
)

func decode(w http.ResponseWriter, r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return errors.New("expected one JSON object")
	}
	return nil
}

func (s *Server) deviceRequest(ctx context.Context, user identity.User, device, method, operation, lease string, body any) ([]byte, int, error) {
	id, serial, ok := splitDeviceID(device)
	if !ok {
		return nil, 400, errors.New("invalid device ID")
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return nil, 400, err
		}
	}
	return deviceResult(s.agentRequest(ctx, user, id, serial, method, operation, lease, data, false, 20<<20))
}

func (s *Server) agentRequest(ctx context.Context, user identity.User, id, serial, method, operation, lease string, data []byte, adminPath bool, limit int64) ([]byte, int, error) {
	return s.agentRequestWithQuery(ctx, user, id, serial, method, operation, lease, data, adminPath, limit, "", nil)
}

func (s *Server) agentRequestWithQuery(ctx context.Context, user identity.User, id, serial, method, operation, lease string, data []byte, adminPath bool, limit int64, rawQuery string, headers *http.Header) ([]byte, int, error) {
	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil || p.session.IsClosed() {
		return nil, 503, errors.New("device agent is offline")
	}
	target := url.URL{Scheme: "http", Host: "localhost", Path: deviceRoute(serial, operation, adminPath), RawQuery: rawQuery}
	r, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(data))
	if err != nil {
		return nil, 400, err
	}
	forwardIdentity(r, user)
	r.Header.Set("Content-Type", "application/json")
	if lease != "" {
		r.Header.Set("X-Lease-Token", lease)
	}
	response, err := p.transport.RoundTrip(r)
	if err != nil {
		return nil, 502, errors.New("agent request failed")
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, 502, errors.New("invalid agent response")
	}
	if headers != nil {
		*headers = response.Header
	}
	return data, response.StatusCode, nil
}

func deviceResult(data []byte, status int, err error) ([]byte, int, error) {
	if err != nil {
		return nil, status, err
	}
	if status < 200 || status >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(status)
		}
		return nil, status, errors.New(failure.Error)
	}
	return data, status, nil
}

func validateSelection(in client.Selection) error {
	if in.Platform != "" && in.Platform != "android" {
		return errors.New("only the android platform is supported")
	}
	if in.Kind != "" && in.Kind != "physical" && in.Kind != "virtual" {
		return errors.New("kind must be physical or virtual")
	}
	if len(in.Tags) > 32 {
		return errors.New("at most 32 tags are allowed")
	}
	for _, tag := range in.Tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 100 {
			return errors.New("tags must contain 1–100 bytes")
		}
	}
	return nil
}

func matches(device client.Device, in client.Selection) bool {
	if device.State != "device" || device.Occupied {
		return false
	}
	if in.Platform != "" && device.Platform != in.Platform {
		return false
	}
	if in.Kind != "" && device.Kind != in.Kind {
		return false
	}
	if in.Model != "" && !strings.EqualFold(in.Model, device.Model) {
		return false
	}
	if in.DeviceID != "" && in.DeviceID != device.Serial {
		return false
	}
	for _, tag := range in.Tags {
		found := false
		for _, available := range device.Tags {
			if tag == available {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Server) allocate(ctx context.Context, user identity.User, selection client.Selection, ttl int) (client.Assignment, int, error) {
	var pick string
	var expiry int64
	if strings.HasPrefix(selection.DeviceID, poolPickPrefix) {
		pick = selection.DeviceID
		selection.DeviceID = ""
		var ok bool
		expiry, ok = poolPickExpiry(pick)
		if !ok {
			return client.Assignment{}, 403, errors.New(deviceUnavailable)
		}
	}
	// Only the allocator sees unassigned inventory. Ordinary inventory requests
	// keep the caller's identity and return that account's assigned device only.
	inventoryUser := identity.User{Subject: "marionette:allocator", Role: "admin"}
	request, _ := http.NewRequestWithContext(identity.WithUser(ctx, inventoryUser), "GET", "http://localhost/api/devices", nil)
	var candidates []client.Device
	unavailable := false
	for _, result := range s.collect(request, "/api/devices") {
		var inventory struct {
			Devices []client.Device `json:"devices"`
		}
		if result.err != nil || json.Unmarshal(result.body, &inventory) != nil {
			unavailable = true
			continue
		}
		for _, device := range inventory.Devices {
			device.RawSerial, device.AgentID = device.Serial, result.id
			device.Serial = DeviceID(result.id, device.Serial)
			if matches(device, selection) && (pick == "" || hmac.Equal([]byte(pick), []byte(s.poolPick(user.Subject, device.Serial, expiry)))) {
				candidates = append(candidates, device)
			}
		}
	}
	if len(candidates) == 0 {
		if unavailable {
			return client.Assignment{}, 503, errors.New("matching device unavailable; one or more agents could not be reached")
		}
		if pick != "" {
			return client.Assignment{}, 403, errors.New(deviceUnavailable)
		}
		return client.Assignment{}, 409, errors.New("no available device matches the requested conditions")
	}
	s.mu.Lock()
	offset := int(s.next % uint64(len(candidates)))
	s.next++
	s.mu.Unlock()
	var lastErr error
	for i := range candidates {
		device := candidates[(i+offset)%len(candidates)]
		data, status, err := s.claimDevice(ctx, user, device.Serial, ttl)
		if err != nil {
			if errors.Is(err, errAlreadyAssigned) {
				return client.Assignment{}, status, err
			}
			if status == 409 {
				continue
			} // Inventory is advisory; only the agent's atomic claim grants ownership.
			lastErr = err
			continue
		}
		var lease client.Lease
		if err := json.Unmarshal(data, &lease); err != nil || lease.Token == "" {
			return client.Assignment{}, 502, errors.New("invalid lease response")
		}
		_, status, err = s.deviceRequest(ctx, user, device.Serial, "POST", "start", lease.Token, nil)
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			releaseErr := s.releaseDevice(cleanup, user, device.Serial, lease)
			cancel()
			if releaseErr != nil {
				return client.Assignment{}, status, fmt.Errorf("controller startup failed; lease expires at %s: %w", lease.ExpiresAt.Format(time.RFC3339), err)
			}
			return client.Assignment{}, status, err
		}
		device.Occupied, device.Owner, device.LeaseID, device.ExpiresAt = true, lease.Owner, lease.ID, &lease.ExpiresAt
		device.Mine = user.Subject != ""
		return client.Assignment{Assigned: device.Serial, Device: device, Lease: lease}, 200, nil
	}
	if lastErr != nil {
		return client.Assignment{}, 502, lastErr
	}
	return client.Assignment{}, 409, errors.New("matching devices were claimed concurrently; retry occupancy")
}

func (s *Server) occupy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		client.Selection
		TTLSeconds int `json:"ttlSeconds"`
	}
	if err := decode(w, r, &in); err != nil {
		respond(w, 400, map[string]string{"error": "invalid occupancy JSON"})
		return
	}
	if err := validateSelection(in.Selection); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.leases.Duration(in.TTLSeconds); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	user, _ := identity.FromContext(r.Context())
	if in.DeviceID != "" && !strings.HasPrefix(in.DeviceID, poolPickPrefix) && !identity.IsAdmin(r.Context()) && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		respond(w, 403, map[string]string{"error": deviceUnavailable})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if devices, _ := s.assignments(ctx, user); len(devices) != 0 {
		respond(w, 409, map[string]string{"error": errAlreadyAssigned.Error()})
		return
	}
	assignment, status, err := s.allocate(ctx, user, in.Selection, in.TTLSeconds)
	if err != nil {
		respond(w, status, map[string]string{"error": err.Error()})
		return
	}
	respond(w, status, assignment)
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := decode(w, r, &in); err != nil || in == nil {
		respond(w, 400, map[string]string{"error": "invalid action JSON"})
		return
	}
	device, _ := in["deviceId"].(string)
	delete(in, "deviceId")
	action := r.PathValue("action")
	switch action {
	case "click":
		if _, ok := in["selector"]; !ok {
			action = "tap"
		}
	case "input":
		action = "text"
	case "dump":
		action = "uiTree"
	case "tap", "swipe", "text", "key", "findElement", "uiTree", "info":
	default:
		respond(w, 400, map[string]string{"error": "unsupported action"})
		return
	}
	in["action"] = action
	user, _ := identity.FromContext(r.Context())
	admin := identity.IsAdmin(r.Context())
	if !admin {
		if _, ok := s.checkDeviceAccess(w, r, user, device); !ok {
			return
		}
	}
	data, status, err := s.deviceRequest(r.Context(), user, device, "POST", "actions", r.Header.Get("X-Lease-Token"), in)
	if err != nil {
		if !admin && (status == 403 || status == 404) {
			respond(w, 403, map[string]string{"error": deviceUnavailable})
			return
		}
		respond(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeDeviceJSON(w, status, data)
}

const (
	poolPickPrefix = "pick_"
	poolPickTTL    = 10 * time.Minute
)

func (s *Server) poolPick(subject, device string, expires int64) string {
	stamp := strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, s.poolKey[:])
	_, _ = fmt.Fprintf(mac, "%s:%d:%s%s", stamp, len(subject), subject, device)
	return poolPickPrefix + stamp + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func poolPickExpiry(pick string) (int64, bool) {
	stamp, mac, ok := strings.Cut(strings.TrimPrefix(pick, poolPickPrefix), ":")
	expires, err := strconv.ParseInt(stamp, 10, 64)
	now := time.Now()
	return expires, ok && err == nil && len(mac) == base64.RawURLEncoding.EncodedLen(sha256.Size) && expires > now.Unix() && expires <= now.Add(poolPickTTL).Unix()
}

// pool describes unassigned devices with account-bound selection IDs.
func (s *Server) pool(w http.ResponseWriter, r *http.Request) {
	user, _ := identity.FromContext(r.Context())
	expires := time.Now().Add(poolPickTTL).Unix()
	w.Header().Set("Cache-Control", "no-store")
	inventoryUser := identity.User{Subject: "marionette:allocator", Role: "admin"}
	request, _ := http.NewRequestWithContext(identity.WithUser(r.Context(), inventoryUser), "GET", "http://localhost/api/devices", nil)
	type option struct {
		ID             string `json:"id"`
		Model          string `json:"model"`
		Name           string `json:"name,omitempty"`
		AndroidVersion string `json:"androidVersion,omitempty"`
		SDK            int    `json:"sdk,omitempty"`
		Width          int    `json:"width,omitempty"`
		Height         int    `json:"height,omitempty"`
	}
	type summary struct {
		Total          int      `json:"total"`
		Available      int      `json:"available"`
		Cleaning       int      `json:"cleaning,omitempty"`
		CleaningFailed int      `json:"cleaningFailed,omitempty"`
		Devices        []option `json:"devices,omitempty"`
	}
	var virtual, physical summary
	unavailable := false
	for _, result := range s.collect(request, "/api/devices") {
		var inventory struct {
			Devices []client.Device `json:"devices"`
		}
		if result.err != nil || json.Unmarshal(result.body, &inventory) != nil {
			unavailable = true
			continue
		}
		for _, device := range inventory.Devices {
			if device.Platform != "android" {
				continue
			}
			target := &physical
			if device.Kind == "virtual" {
				target = &virtual
			}
			switch device.State {
			case "cleaning":
				target.Cleaning++
				continue
			case "cleaning_failed":
				target.CleaningFailed++
				continue
			case "device":
			default:
				continue
			}
			target.Total++
			if !device.Occupied {
				target.Available++
				target.Devices = append(target.Devices, option{
					ID: s.poolPick(user.Subject, DeviceID(result.id, device.Serial), expires), Model: strings.ReplaceAll(device.Model, "_", " "), Name: device.Name,
					AndroidVersion: device.AndroidVersion, SDK: device.SDK, Width: device.Width, Height: device.Height,
				})
			}
		}
	}
	for _, group := range []*summary{&virtual, &physical} {
		devices := group.Devices
		sort.Slice(devices, func(i, j int) bool { return devices[i].Model+devices[i].ID < devices[j].Model+devices[j].ID })
	}
	respond(w, 200, map[string]any{"virtual": virtual, "physical": physical, "partial": unavailable})
}
