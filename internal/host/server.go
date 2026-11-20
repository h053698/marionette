package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"marionette/internal/identity"
	"marionette/internal/machine"
	"marionette/internal/service"
)

type Config struct {
	ADB               ADB
	DeviceJar         string
	DiscoveryInterval time.Duration
	PublicOrigin      string
	// AllowedOrigins are separately hosted frontends whose browser requests and
	// WebSocket handshakes are accepted. Authentication is enforced by the caller.
	AllowedOrigins                       []string
	Tags                                 []string
	Machine                              *machine.Manager
	Leases                               service.LeasePolicy
	MaxUploadBytes                       int64
	MaxDownloadBytes                     int64
	MaxLeaseTransfers, MaxAgentTransfers int
	MaxStagingBytes                      int64
	WipeTimeout                          time.Duration
	WipeKeepPackages                     []string
	// ICEServers are offered to direct ADB peers; nil gathers host candidates only.
	ICEServers []webrtc.ICEServer
}
type Device struct {
	Platform             string     `json:"platform"`
	Tags                 []string   `json:"tags"`
	Serial               string     `json:"serial"`
	Model                string     `json:"model"`
	State                string     `json:"state"`
	Occupied             bool       `json:"occupied"`
	Owner                string     `json:"owner"`
	ExpiresAt            *time.Time `json:"expiresAt"`
	LeaseID              string     `json:"leaseId,omitempty"`
	Kind                 string     `json:"kind"`
	Managed              bool       `json:"managed"`
	ControllerReady      bool       `json:"controllerReady"`
	VideoProfile         string     `json:"videoProfile,omitempty"`
	Viewers              int        `json:"viewers"`
	Mine                 bool       `json:"mine"`
	Name                 string     `json:"name,omitempty"`
	AndroidVersion       string     `json:"androidVersion,omitempty"`
	SDK                  int        `json:"sdk,omitempty"`
	Width                int        `json:"width,omitempty"`
	Height               int        `json:"height,omitempty"`
	ConnectionManageable bool       `json:"connectionManageable,omitempty"`
	DebugRequested       bool       `json:"debugRequested,omitempty"`
	AdminControlModes    bool       `json:"adminControlModes,omitempty"`
	LeaseMode            string     `json:"leaseMode,omitempty"`
	transportID          string
}

func (d Device) target() adbTarget { return adbTarget{d.Serial, d.transportID} }

type lease struct {
	ID           string    `json:"id"`
	Token        string    `json:"token"`
	Owner        string    `json:"owner"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Mode         string    `json:"mode,omitempty"`
	ctx          context.Context
	cancel       context.CancelFunc
	done         <-chan struct{}
	target       adbTarget
	epoch        uint64
	inputRunners map[*Runner]struct{}
	rotated      bool
	controls     map[*controlSession]struct{}
	transfers    int
	videoStreams int
	subject      string
	handoffHash  [32]byte
	handoffUntil time.Time
}
type slot struct {
	controls            int
	device              Device
	lease               *lease
	runner              *Runner
	work                *runtimeWork
	revision            uint64
	previews            int
	interactive         bool
	profile             string
	online              chan struct{}
	reset               bool
	deviceCalls         int
	adbStreams          int
	directPeers         int
	leaseEpoch          uint64
	leaseCleanup        chan struct{}
	wipePending         bool
	wipeCancel          context.CancelFunc
	lastRelease         releaseReceipt
	blockedTarget       *adbTarget
	replacing           bool
	videoStreams        int
	fileCommit          chan struct{}
	connectionChanging  bool
	adbTransportPresent bool
	debugRequestedUntil time.Time
}
type Server struct {
	cfg                     Config
	deviceAllow             []string // Used only when no machine manager is configured.
	mu                      sync.Mutex
	devices                 map[string]*slot
	slots                   map[string]*slot // Retain lifecycle coordination across disappear/reappear.
	adbAvailable            bool
	closed                  bool
	cancel                  context.CancelFunc
	wg                      sync.WaitGroup
	cleanup                 sync.WaitGroup
	handler                 http.Handler
	directOnce              sync.Once
	direct                  *webrtc.API
	videoStreams, transfers int
	stagedBytes             int64
}

const DefaultDiscoveryInterval = 2 * time.Second

func New(cfg Config) *Server {
	if cfg.ADB == nil {
		cfg.ADB = ExecADB{}
	}
	if cfg.DiscoveryInterval <= 0 {
		cfg.DiscoveryInterval = DefaultDiscoveryInterval
	}
	if cfg.Leases.DefaultTTL == 0 {
		cfg.Leases.DefaultTTL = service.DefaultLeaseTTL
	}
	if cfg.Leases.MaxTTL == 0 {
		cfg.Leases.MaxTTL = service.DefaultMaxLease
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if cfg.MaxDownloadBytes <= 0 {
		cfg.MaxDownloadBytes = DefaultMaxDownloadBytes
	}
	if cfg.MaxLeaseTransfers <= 0 {
		cfg.MaxLeaseTransfers = DefaultMaxLeaseTransfers
	}
	if cfg.MaxAgentTransfers <= 0 {
		cfg.MaxAgentTransfers = DefaultMaxAgentTransfers
	}
	if cfg.MaxStagingBytes <= 0 {
		cfg.MaxStagingBytes = DefaultMaxStagingBytes
	}
	if cfg.WipeTimeout <= 0 {
		cfg.WipeTimeout = DefaultWipeTimeout
	}
	s := &Server{cfg: cfg, devices: make(map[string]*slot), slots: make(map[string]*slot)}
	if cfg.Machine == nil {
		s.deviceAllow = service.List(os.Getenv("MARIONETTE_DEVICE_ALLOW"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/devices", s.list)
	mux.HandleFunc("GET /api/assignments", s.list)
	mux.HandleFunc("GET /api/agent", s.agentStatus)
	mux.HandleFunc("GET /api/admin/leases", s.adminLeases)
	mux.HandleFunc("PUT /api/admin/agent", s.agentPolicy)
	mux.HandleFunc("POST /api/devices/{serial}/lease", s.claim)
	mux.HandleFunc("PUT /api/devices/{serial}/lease", s.renew)
	mux.HandleFunc("DELETE /api/devices/{serial}/lease", s.release)
	mux.HandleFunc("POST /api/devices/{serial}/start", s.start)
	mux.HandleFunc("POST /api/devices/{serial}/actions", s.action)
	mux.HandleFunc("GET /api/devices/{serial}/screenshot", s.screenshot)
	mux.HandleFunc("GET /api/devices/{serial}/video", s.video)
	mux.HandleFunc("GET /api/devices/{serial}/control", s.control)
	mux.HandleFunc("GET /api/devices/{serial}/adb", s.adbRelay)
	mux.HandleFunc("POST /api/devices/{serial}/direct", s.directADB)
	mux.HandleFunc("GET /api/devices/{serial}/files", s.fileList)
	mux.HandleFunc("PUT /api/devices/{serial}/files", s.fileUpload)
	mux.HandleFunc("POST /api/devices/{serial}/files", s.fileMkdir)
	mux.HandleFunc("DELETE /api/devices/{serial}/files", s.fileDelete)
	mux.HandleFunc("GET /api/devices/{serial}/download", s.fileDownload)
	mux.HandleFunc("POST /api/devices/{serial}/install", s.appInstall)
	mux.HandleFunc("POST /api/devices/{serial}/uninstall", s.appUninstall)
	mux.HandleFunc("POST /api/devices/{serial}/handoff", s.handoff)
	mux.HandleFunc("POST /api/devices/{serial}/attach", s.attach)
	mux.HandleFunc("GET /api/admin/devices/{serial}/preview", s.preview)
	mux.HandleFunc("POST /api/admin/devices/{serial}/keyframe", s.previewKeyframe)
	mux.HandleFunc("DELETE /api/admin/devices/{serial}/lease", s.adminLease)
	mux.HandleFunc("POST /api/admin/devices/{serial}/lease", s.adminLease)
	mux.HandleFunc("POST /api/admin/devices/{serial}/control-mode", s.adminControlLease)
	mux.HandleFunc("POST /api/admin/devices/{serial}/cleanup", s.adminCleanup)
	mux.HandleFunc("POST /api/admin/devices/{serial}/restore", s.adminRestore)
	mux.HandleFunc("POST /api/admin/devices/{serial}/connection", s.adminDeviceConnection)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "unknown API endpoint") })
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		public, valid := publicOrigin(cfg.PublicOrigin)
		if !localHost(r.Host) && !(valid && strings.EqualFold(r.Host, public.Host)) {
			fail(w, 403, "non-loopback Host rejected")
			return
		}
		if !s.sameOrigin(r) {
			fail(w, 403, "cross-origin request rejected")
			return
		}
		mux.ServeHTTP(w, r)
	})
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }
func publicOrigin(value string) (*url.URL, bool) {
	u, err := url.Parse(value)
	return u, err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
func (s *Server) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		for _, allowed := range s.cfg.AllowedOrigins {
			if strings.EqualFold(strings.TrimSpace(allowed), origin) {
				return true
			}
		}
	}
	if u, ok := publicOrigin(s.cfg.PublicOrigin); ok && strings.EqualFold(r.Host, u.Host) {
		return r.Header.Get("Origin") == "" || r.Header.Get("Origin") == s.cfg.PublicOrigin
	}
	return sameOrigin(r)
}
func localHost(authority string) bool {
	host := authority
	if h, port, err := net.SplitHostPort(authority); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		host = h
	} else if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = authority[1 : len(authority)-1]
	} else if strings.ContainsAny(authority, ":[]") {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, e := url.Parse(o)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return e == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxControl)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Start performs initial discovery synchronously, then maintains device and lease
// state independently of API traffic.
func (s *Server) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.discover(ctx)
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(s.cfg.DiscoveryInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				func() { defer logPanic("device discovery"); s.discover(ctx) }()
			}
		}
	}()
	// A slow adb discovery must never delay revocation of an expired lease.
	go func() {
		defer s.wg.Done()
		expiry := time.NewTicker(100 * time.Millisecond)
		defer expiry.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-expiry.C:
				func() {
					defer logPanic("lease expiry")
					s.mu.Lock()
					defer s.mu.Unlock()
					s.expireLocked()
				}()
			}
		}
	}()
}
func (s *Server) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	s.mu.Lock()
	s.closed = true
	for _, d := range s.slots {
		if d.work != nil {
			d.work.cancel()
		}
		s.invalidateLocked(d)
		if d.leaseCleanup == nil {
			s.disposeLocked(d.runner)
		}
		d.runner = nil
	}
	s.mu.Unlock()
	s.cleanup.Wait()
}
func (s *Server) disposeLocked(r *Runner) {
	if r == nil {
		return
	}
	s.cleanup.Add(1)
	go func() { defer s.cleanup.Done(); defer logPanic("dispose runner"); r.Close() }()
}
func (s *Server) invalidateLocked(d *slot) {
	s.revokeLeaseLocked(d)
	if d.wipeCancel != nil {
		d.wipeCancel()
	}
	if d.online != nil {
		close(d.online)
		d.online = nil
		d.reset = true
	}
}
func (s *Server) revokeLeaseLocked(d *slot) <-chan struct{} {
	l := d.lease
	if l == nil {
		return d.leaseCleanup
	}
	for c := range l.controls {
		c.close()
	}
	l.cancel()
	d.lease = nil
	d.interactive = false
	if len(l.inputRunners) == 0 {
		s.reconcileLocked(d)
		return nil
	}
	done := make(chan struct{})
	d.leaseCleanup = done
	runner := d.runner
	s.cleanup.Add(1)
	go func() {
		defer s.cleanup.Done()
		ok := false
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !ok {
				d.blockedTarget = &l.target
				if s.cfg.Machine != nil {
					s.cfg.Machine.Quarantine(l.target.serial, l.target.transportID)
				}
			}
			d.leaseCleanup = nil
			if s.closed {
				s.disposeLocked(runner)
			} else {
				s.reconcileLocked(d)
			}
			close(done)
		}()
		defer logPanic("lease input cleanup")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for inputRunner := range l.inputRunners {
			if err := inputRunner.resetInput(ctx, l.epoch); err != nil {
				slog.Warn("lease input cleanup failed", "serial", l.target.serial, "error", err)
				return
			}
		}
		if l.rotated {
			if _, err := s.cfg.ADB.Run(ctx, l.target.args("shell", "cmd window user-rotation lock 0 && cmd window user-rotation free")...); err != nil {
				slog.Warn("lease rotation cleanup failed", "serial", l.target.serial, "error", err)
				return
			}
		}
		ok = true
	}()
	return done
}

func logPanic(operation string) {
	if p := recover(); p != nil {
		slog.Error("agent worker panicked", "operation", operation, "panicType", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
	}
}

func (d *slot) grant(owner, subject string, duration time.Duration) *lease {
	d.leaseEpoch++
	ctx, cancel := context.WithCancel(context.Background())
	d.lease = &lease{ID: randomID(), Token: randomID(), Owner: owner, subject: subject,
		ExpiresAt: time.Now().Add(duration), ctx: ctx, cancel: cancel, done: ctx.Done(),
		target: d.device.target(), epoch: d.leaseEpoch, controls: make(map[*controlSession]struct{}),
		inputRunners: make(map[*Runner]struct{})}
	return d.lease
}

func (s *Server) canGrantLocked(d *slot) bool {
	target := d.device.target()
	return !s.closed && s.deviceOnlineLocked(d) && s.allows(d.device.Serial) &&
		s.wipeStateLocked(d) == "" && !s.quarantinedLocked(d) && d.leaseCleanup == nil && !d.reset &&
		(d.runner == nil || d.runner.target == target)
}

func (s *Server) quarantinedLocked(d *slot) bool {
	target := d.device.target()
	return (d.blockedTarget != nil && *d.blockedTarget == target) ||
		(s.cfg.Machine != nil && s.cfg.Machine.Quarantined(target.serial, target.transportID))
}

func (s *Server) allows(serial string) bool {
	if s.cfg.Machine != nil {
		return s.cfg.Machine.Allows(serial)
	}
	return len(s.deviceAllow) == 0 || machine.MatchDeviceAllow(s.deviceAllow, serial, false)
}

func (s *Server) disconnected(serial string) bool {
	return s.cfg.Machine != nil && s.cfg.Machine.Disconnected(serial)
}

func (s *Server) deviceOnlineLocked(d *slot) bool {
	return d.device.State == "device" && !d.connectionChanging && !s.disconnected(d.device.Serial)
}

// Caller holds mu. Release it while waiting so other devices can still expire.
func (s *Server) waitCleanupLocked(ctx context.Context, done <-chan struct{}) {
	if done == nil {
		return
	}
	s.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.mu.Lock()
}
func (s *Server) expireLocked() {
	now := time.Now()
	for _, d := range s.devices {
		if d.lease != nil && !now.Before(d.lease.ExpiresAt) {
			s.revokeLeaseLocked(d)
		}
	}
}
func (s *Server) discover(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	b, err := s.cfg.ADB.Run(ctx, "devices", "-l")
	found := map[string]Device{}
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if strings.Contains(" "+line+" ", " device:marionette_remote ") {
				continue
			}
			if len(f) < 2 || f[0] == "List" || strings.HasPrefix(f[0], "*") {
				continue
			}
			d := Device{Serial: f[0], State: f[1], Platform: "android", Tags: append([]string{}, s.cfg.Tags...)}
			for _, v := range f[2:] {
				key, value, _ := strings.Cut(v, ":")
				switch key {
				case "model":
					d.Model = value
				case "transport_id":
					if id, err := strconv.ParseUint(value, 10, 64); err == nil && id != 0 {
						d.transportID = strconv.FormatUint(id, 10)
					}
				}
			}
			if _, execADB := s.cfg.ADB.(ExecADB); execADB && d.State == "device" && d.transportID == "" {
				d.State = "offline"
			}
			found[d.Serial] = d
		}
	}
	if usb, ok := s.cfg.ADB.(interface {
		USBDevices(context.Context) ([]Device, error)
	}); ok {
		devices, _ := usb.USBDevices(ctx)
		for _, device := range devices {
			if _, exists := found[device.Serial]; !exists {
				device.Tags = append([]string{}, s.cfg.Tags...)
				found[device.Serial] = device
			}
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.adbAvailable = err == nil
	for serial, d := range s.devices {
		newD, ok := found[serial]
		d.adbTransportPresent = ok && newD.transportID != ""
		allowed := s.allows(serial)
		if !ok || newD.State != "device" || newD.target() != d.device.target() || !allowed {
			d.device.State = "offline"
			s.invalidateLocked(d)
		}
		if !allowed {
			delete(s.devices, serial)
			s.reconcileLocked(d)
		}
	}
	var specs []Device
	for serial, d := range found {
		if !s.allows(serial) {
			continue
		}
		if strings.HasPrefix(serial, "emulator-") {
			d.Kind = "virtual"
		} else {
			d.Kind = "physical"
		}
		if s.cfg.Machine != nil {
			d.Managed = s.cfg.Machine.Owns(serial)
		}
		old := s.slots[serial]
		if old == nil {
			old = &slot{fileCommit: make(chan struct{}, 1)}
			s.slots[serial] = old
		}
		if d.State != "device" && old.device.Model != "" {
			d.Model = old.device.Model
		}
		old.adbTransportPresent = d.transportID != ""
		if d.State != "device" && d.transportID == "" {
			d.transportID = old.device.transportID
		}
		if d.target() == old.device.target() {
			d.Name, d.AndroidVersion, d.SDK, d.Width, d.Height = old.device.Name, old.device.AndroidVersion, old.device.SDK, old.device.Width, old.device.Height
		}
		old.device = d
		if s.deviceOnlineLocked(old) && old.online == nil {
			old.online = make(chan struct{})
			old.debugRequestedUntil = time.Time{}
		}
		if s.deviceOnlineLocked(old) && d.AndroidVersion == "" {
			specs = append(specs, d)
		}
		s.devices[serial] = old
	}
	for _, d := range s.devices {
		if desired := s.desiredLocked(d); d.reset || desired != d.profile || d.runner != nil && !d.runner.alive() {
			s.reconcileLocked(d)
		}
	}
	s.mu.Unlock()
	for _, d := range specs {
		if ctx.Err() != nil {
			break
		}
		s.readSpec(ctx, &d)
		s.mu.Lock()
		if current := s.devices[d.Serial]; current != nil && current.device.target() == d.target() {
			current.device.Name, current.device.AndroidVersion, current.device.SDK, current.device.Width, current.device.Height = d.Name, d.AndroidVersion, d.SDK, d.Width, d.Height
		}
		s.mu.Unlock()
	}
}

// readSpec asks a ready device for its marketing name, Android version and
// screen size once, so members can tell devices apart. Missing values are
// simply left empty.
func (s *Server) readSpec(ctx context.Context, d *Device) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := s.cfg.ADB.Run(ctx, d.target().args("shell", "getprop ro.build.version.release; getprop ro.build.version.sdk; getprop ro.product.marketname; getprop ro.product.model; wm size")...)
	if err != nil {
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	field := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	if release := field(0); release != "" && len(release) <= 16 {
		d.AndroidVersion = release
	}
	if sdk, err := strconv.Atoi(field(1)); err == nil && sdk > 0 && sdk < 1000 {
		d.SDK = sdk
	}
	name := field(2)
	if name == "" {
		name = field(3)
	}
	if len(name) <= 80 {
		d.Name = name
	}
	for _, line := range lines[min(4, len(lines)):] {
		if i := strings.Index(line, "Physical size:"); i >= 0 {
			var w, h int
			if _, err := fmt.Sscanf(strings.TrimSpace(line[i+len("Physical size:"):]), "%dx%d", &w, &h); err == nil && w > 0 && h > 0 && w < 20000 && h < 20000 {
				d.Width, d.Height = w, h
			}
		}
	}
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	available := s.adbAvailable
	s.mu.Unlock()
	st, e := os.Stat(s.cfg.DeviceJar)
	writeJSON(w, 200, map[string]any{"version": "1", "adbAvailable": available, "deviceJarAvailable": e == nil && st.Mode().IsRegular()})
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.expireLocked()
	user, authenticated := identity.FromContext(r.Context())
	admin := identity.IsAdmin(r.Context())
	assignmentLookup := r.URL.Path == "/api/assignments"
	private := assignmentLookup || (authenticated && !admin)
	devices := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		mine := d.lease != nil && d.lease.ownedBy(r.Context())
		// The relay uses this private snapshot to enforce one device per subject,
		// even when that subject makes a request using a lower-privilege token.
		if assignmentLookup && authenticated && d.lease != nil && d.lease.subject == user.Subject {
			mine = true
		}
		if private && !mine {
			continue
		}
		if !s.allows(d.device.Serial) {
			continue
		}
		v := d.device
		v.ConnectionManageable = s.cfg.Machine != nil && v.Kind == "physical"
		v.AdminControlModes = admin
		v.DebugRequested = time.Now().Before(d.debugRequestedUntil)
		if s.disconnected(v.Serial) {
			v.State = "disconnected"
		} else if d.connectionChanging {
			v.State = "reconnecting"
		}
		if state := s.wipeStateLocked(d); state != "" {
			v.State = state
		}
		v.Mine = mine
		v.ControllerReady = d.runner != nil && d.runner.alive()
		v.VideoProfile = d.profile
		if v.ControllerReady {
			v.Viewers = d.runner.viewerCount()
		}
		if d.lease != nil {
			v.Occupied = true
			v.Owner = d.lease.Owner
			v.LeaseID = d.lease.ID
			v.LeaseMode = d.lease.Mode
			expiry := d.lease.ExpiresAt
			v.ExpiresAt = &expiry
		}
		devices = append(devices, v)
	}
	s.mu.Unlock()
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	writeJSON(w, 200, map[string]any{"devices": devices})
}
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Owner   string `json:"owner"`
		TTL     int    `json:"ttlSeconds"`
		Resume  *bool  `json:"resume"`
		LeaseID string `json:"leaseId"`
	}
	if e := readJSON(w, r, &body); e != nil {
		fail(w, 400, "invalid lease JSON")
		return
	}
	duration, e := s.cfg.Leases.Duration(body.TTL)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	body.Owner = strings.TrimSpace(body.Owner)
	user, authenticated := identity.FromContext(r.Context())
	if authenticated {
		body.Owner = strings.TrimSpace(user.Name)
		if body.Owner == "" {
			body.Owner = strings.TrimSpace(user.Email)
		}
		if body.Owner == "" {
			body.Owner = user.Subject
		}
	}
	if body.Owner == "" || len(body.Owner) > 200 {
		fail(w, 400, "owner must contain 1–200 bytes")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	d := s.devices[r.PathValue("serial")]
	if r.Context().Err() != nil {
		fail(w, 408, "device claim canceled")
		return
	}
	if !s.allows(r.PathValue("serial")) {
		fail(w, 409, "this device type is disabled by the agent operation policy")
		return
	}
	if s.closed {
		fail(w, 503, "server shutting down")
		return
	}
	if d == nil {
		fail(w, 404, "unknown device")
		return
	}
	if s.wipeStateLocked(d) != "" {
		fail(w, 409, "device cleanup is incomplete")
		return
	}
	s.waitCleanupLocked(r.Context(), d.leaseCleanup)
	if r.Context().Err() != nil {
		fail(w, 408, "device claim canceled")
		return
	}
	if d.replacing || s.devices[r.PathValue("serial")] != d || !s.canGrantLocked(d) {
		fail(w, 409, "device is unavailable or lease cleanup is incomplete")
		return
	}
	if d.device.State != "device" {
		fail(w, 409, "device is not authorized and online")
		return
	}
	if body.LeaseID != "" && (d.lease == nil || d.lease.ID != body.LeaseID || !d.lease.ownedBy(r.Context())) {
		fail(w, 409, "device assignment changed; refresh and try again")
		return
	}
	if d.lease != nil {
		if d.lease.ownedBy(r.Context()) && (body.Resume == nil || *body.Resume) {
			writeJSON(w, 200, d.lease)
			return
		}
		fail(w, 409, "device already leased")
		return
	}
	if authenticated {
		for _, other := range s.devices {
			if other.lease != nil && other.lease.subject == user.Subject {
				fail(w, 409, "return your current device before acquiring another")
				return
			}
		}
	}
	writeJSON(w, 201, d.grant(body.Owner, user.Subject, duration))
}

// authorizedLocked must run under mu; token comparisons never grant access to
// an expired lease even between periodic expiry ticks.
func (s *Server) authorizedLocked(w http.ResponseWriter, r *http.Request, token string) *slot {
	d, status, message := s.authorizeLocked(r, token)
	if d == nil {
		fail(w, status, message)
	}
	return d
}

func (l *lease) ownedBy(ctx context.Context) bool {
	user, authenticated := identity.FromContext(ctx)
	return authenticated && user.Subject == l.subject && (l.Mode == "" || identity.IsAdmin(ctx))
}

func (s *Server) authorizeLocked(r *http.Request, token string) (*slot, int, string) {
	s.expireLocked()
	if s.closed {
		return nil, 503, "server shutting down"
	}
	d := s.devices[r.PathValue("serial")]
	if d == nil {
		return nil, 404, "unknown device"
	}
	if d.lease == nil || token == "" || d.lease.Token != token {
		return nil, 403, "invalid or expired lease token"
	}
	if (d.lease.subject != "" || d.lease.Mode != "") && !d.lease.ownedBy(r.Context()) {
		return nil, 403, "lease belongs to another identity"
	}
	if d.device.State != "device" {
		return nil, 409, "device unavailable"
	}
	if !s.allows(d.device.Serial) {
		s.invalidateLocked(d)
		return nil, 409, "device unavailable"
	}
	return d, 200, ""
}
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TTL int `json:"ttlSeconds"`
	}
	if e := readJSON(w, r, &body); e != nil {
		fail(w, 400, "invalid lease JSON")
		return
	}
	duration, e := s.cfg.Leases.Duration(body.TTL)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		return
	}
	if d.lease.Mode != leaseTemporary {
		d.lease.ExpiresAt = time.Now().Add(duration)
	}
	writeJSON(w, 200, d.lease)
}
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		s.mu.Unlock()
		return
	}
	d.interactive = true
	l := d.lease
	ready := s.reconcileLocked(d)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	select {
	case <-ready.done:
		if ready.err != nil {
			fail(w, 502, fmt.Sprintf("device start failed: %v", ready.err))
			return
		}
	case <-l.done:
		fail(w, 403, "lease ended during startup")
		return
	case <-ctx.Done():
		fail(w, 504, "device startup canceled or timed out")
		return
	}
	s.mu.Lock()
	valid := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token")) == d
	s.mu.Unlock()
	if !valid {
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var fields map[string]json.RawMessage
	if e := readJSON(w, r, &fields); e != nil {
		fail(w, 400, "invalid action JSON")
		return
	}
	var action string
	_ = json.Unmarshal(fields["action"], &action)
	switch action {
	case "info", "tap", "swipe", "hover", "key", "text", "uiTree", "findElement", "click", "apps", "appIcon", "processes", "launchApp", "rotate", "keyframe":
	default:
		fail(w, 400, "unsupported action")
		return
	}
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.Header.Get("X-Lease-Token"))
	if d == nil {
		s.mu.Unlock()
		return
	}
	runner, l := d.runner, d.lease
	if runner == nil || !runner.alive() {
		s.mu.Unlock()
		fail(w, 409, "start the device runner first")
		return
	}
	if leaseInputAction(action) {
		l.inputRunners[runner] = struct{}{}
		l.rotated = l.rotated || action == "rotate"
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	stopLease := context.AfterFunc(l.ctx, cancel)
	defer stopLease()
	response, err := runner.action(ctx, fields, l)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, response)
}

const (
	maxLeaseVideo    = 4
	maxDeviceVideo   = 32
	maxAgentVideo    = 128
	videoPongTimeout = 45 * time.Second
)

func (s *Server) reserveVideoLocked(w http.ResponseWriter, d *slot, l *lease) bool {
	if l != nil && l.videoStreams >= maxLeaseVideo || d.videoStreams >= maxDeviceVideo || s.videoStreams >= maxAgentVideo {
		fail(w, 429, "too many video connections")
		return false
	}
	if l != nil {
		l.videoStreams++
	}
	d.videoStreams++
	s.videoStreams++
	return true
}

func (s *Server) releaseVideoLocked(d *slot, l *lease) {
	if l != nil {
		l.videoStreams--
	}
	d.videoStreams--
	s.videoStreams--
}

func (s *Server) video(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	d := s.authorizedLocked(w, r, r.URL.Query().Get("token"))
	if d == nil {
		s.mu.Unlock()
		return
	}
	runner, l := d.runner, d.lease
	if runner == nil || !runner.alive() {
		s.mu.Unlock()
		fail(w, 409, "start the device runner first")
		return
	}
	if !s.reserveVideoLocked(w, d, l) {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.releaseVideoLocked(d, l); s.mu.Unlock() }()
	s.stream(w, r, runner, l.done)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, runner *Runner, revoked <-chan struct{}) {
	upgrader := websocket.Upgrader{CheckOrigin: s.sameOrigin, HandshakeTimeout: 5 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	v := runner.subscribe()
	defer runner.unsubscribe(v)
	done := make(chan struct{})
	defer close(done)
	var revokedOnce sync.Once
	closeRevoked := func() {
		revokedOnce.Do(func() {
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "device access ended"), time.Now().Add(time.Second))
			_ = ws.Close()
		})
	}
	defer func() {
		select {
		case <-revoked:
			closeRevoked()
		default:
		}
	}()
	go func() {
		defer logPanic("watch video connection")
		select {
		case <-revoked:
		case <-r.Context().Done():
		case <-runner.done:
		case <-done:
		}
		select {
		case <-revoked:
			closeRevoked()
		default:
			ws.Close()
		}
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer logPanic("read video connection")
		ws.SetReadLimit(1024)
		_ = ws.SetReadDeadline(time.Now().Add(videoPongTimeout))
		ws.SetPongHandler(func(string) error {
			return ws.SetReadDeadline(time.Now().Add(videoPongTimeout))
		})
		for {
			if _, _, e := ws.ReadMessage(); e != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-revoked:
			closeRevoked()
			return
		case <-r.Context().Done():
			return
		case <-runner.done:
			return
		case <-readDone:
			return
		case b := <-v.packets:
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
				return
			}
		case <-ping.C:
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
