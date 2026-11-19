// Package machine supervises only the Android emulators it creates. The agent's
// authenticated connection can restart without interrupting this supervisor.
package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Policy struct {
	Mode          string `json:"mode"`
	EmulatorCount int    `json:"emulatorCount"`
	Image         string `json:"image"`
	KeepAwake     bool   `json:"keepAwake"`
	PreWarm       bool   `json:"preWarm"`
}
type Config struct {
	ID, ADB, SDKRoot, StateDir string
	MaxEmulators               int
	DeviceAllow                []string
}
type Template struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Instance struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
	State  string `json:"state"`
}
type Snapshot struct {
	Policy
	Hostname          string     `json:"hostname"`
	OS                string     `json:"os"`
	Arch              string     `json:"arch"`
	Cores             int        `json:"cores"`
	EmulatorAvailable bool       `json:"emulatorAvailable"`
	MaxEmulators      int        `json:"maxEmulators"`
	Templates         []Template `json:"templates"`
	Instances         []Instance `json:"instances"`
	Error             string     `json:"error,omitempty"`
}
type Manager struct {
	mu                                       sync.Mutex
	cfg                                      Config
	policy                                   Policy
	templates                                []Template
	emulator, avdmanager, javaHome, hostname string
	lock                                     *os.File
	cancel                                   context.CancelFunc
	wg                                       sync.WaitGroup
	wake                                     chan struct{}
	instances                                []Instance
	lastError                                string
	processes                                map[string]*process
	awake                                    map[string]int
	awakeDisabled                            bool
	quarantined                              map[string]string
	wipeRequired                             map[string]bool
}

func New(parent context.Context, cfg Config) (*Manager, error) {
	if cfg.MaxEmulators < 0 || cfg.MaxEmulators > 8 {
		return nil, errors.New("max-emulators must be in 0..8")
	}
	if cfg.ADB == "" {
		cfg.ADB = "adb"
	}
	if path, err := exec.LookPath(cfg.ADB); err == nil {
		cfg.ADB, _ = filepath.Abs(path)
	}
	if cfg.SDKRoot == "" {
		cfg.SDKRoot = os.Getenv("ANDROID_SDK_ROOT")
	}
	if cfg.SDKRoot == "" {
		cfg.SDKRoot = os.Getenv("ANDROID_HOME")
	}
	if cfg.SDKRoot == "" && filepath.Base(filepath.Dir(cfg.ADB)) == "platform-tools" {
		cfg.SDKRoot = filepath.Dir(filepath.Dir(cfg.ADB))
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(".marionette", "agent-"+cfg.ID)
	}
	var err error
	cfg.StateDir, err = filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another agent is already using this state directory")
	}
	m := &Manager{cfg: cfg, lock: lock, policy: Policy{Mode: "mixed", PreWarm: true}, wake: make(chan struct{}, 1), processes: make(map[string]*process), awake: make(map[string]int), quarantined: make(map[string]string)}
	m.hostname, _ = os.Hostname()
	m.emulator = filepath.Join(cfg.SDKRoot, "emulator", "emulator")
	managers, _ := filepath.Glob(filepath.Join(cfg.SDKRoot, "cmdline-tools", "*", "bin", "avdmanager"))
	sort.Strings(managers)
	for _, path := range managers {
		if executable(path) {
			m.avdmanager = path
		}
	}
	m.javaHome = os.Getenv("JAVA_HOME")
	if m.javaHome == "" && executable(filepath.Join(cfg.SDKRoot, "..", "jdk", "bin", "java")) {
		m.javaHome, _ = filepath.Abs(filepath.Join(cfg.SDKRoot, "..", "jdk"))
	}
	images, _ := filepath.Glob(filepath.Join(cfg.SDKRoot, "system-images", "*", "*", "*", "system.img"))
	abi := "x86_64"
	if runtime.GOARCH == "arm64" {
		abi = "arm64-v8a"
	}
	for _, image := range images {
		parts, _ := filepath.Rel(filepath.Join(cfg.SDKRoot, "system-images"), filepath.Dir(image))
		pieces := strings.Split(filepath.ToSlash(parts), "/")
		if len(pieces) == 3 && pieces[2] == abi {
			m.templates = append(m.templates, Template{ID: "system-images;" + strings.Join(pieces, ";"), Name: strings.Join(pieces, " · ")})
		}
	}
	if len(m.templates) > 0 {
		m.policy.Image = m.templates[len(m.templates)-1].ID
	}
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "policy.json"))
	if err == nil {
		if len(raw) > 16<<10 || json.Unmarshal(raw, &m.policy) != nil {
			lock.Close()
			return nil, errors.New("invalid agent policy store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	if err := m.validate(m.policy); err != nil {
		lock.Close()
		return nil, err
	}
	raw, err = os.ReadFile(filepath.Join(cfg.StateDir, "wipe-required.json"))
	if err == nil {
		if len(raw) > 1<<20 || json.Unmarshal(raw, &m.wipeRequired) != nil || m.wipeRequired == nil {
			lock.Close()
			return nil, errors.New("invalid device cleanup store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	if err := m.loadAwake(); err != nil {
		m.awakeDisabled = true
		m.policy.KeepAwake = false
		slog.Warn("KeepAwake disabled until the agent restarts with a valid awake settings store", "error", err)
	}
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	m.wg.Add(1)
	go func() { defer m.wg.Done(); defer logPanic("machine supervisor"); m.run(ctx) }()
	return m, nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0
}
func (m *Manager) available() bool {
	return m.cfg.MaxEmulators > 0 && executable(m.emulator) && executable(m.avdmanager) && len(m.templates) > 0
}
func (m *Manager) validate(p Policy) error {
	if p.KeepAwake && m.awakeDisabled {
		return errors.New("KeepAwake is disabled because the awake settings store is invalid")
	}
	if p.Mode != "physical" && p.Mode != "virtual" && p.Mode != "mixed" {
		return errors.New("mode must be physical, virtual or mixed")
	}
	if p.EmulatorCount < 0 || p.EmulatorCount > m.cfg.MaxEmulators {
		return fmt.Errorf("emulatorCount must be in 0..%d", m.cfg.MaxEmulators)
	}
	if p.Mode == "physical" && p.EmulatorCount != 0 {
		return errors.New("physical mode requires emulatorCount 0")
	}
	if p.Mode == "virtual" && p.KeepAwake {
		return errors.New("keepAwake requires physical or mixed mode")
	}
	if p.EmulatorCount > 0 {
		found := false
		for _, template := range m.templates {
			if template.ID == p.Image {
				found = true
			}
		}
		if !m.available() || !found {
			return errors.New("install Android SDK emulator, command-line tools and a compatible system image on this agent first")
		}
	}
	return nil
}

func writePrivate(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".agent-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	return err
}

func (m *Manager) Update(p Policy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validate(p); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(m.cfg.StateDir, "policy.json"), p); err != nil {
		return err
	}
	m.policy = p
	m.lastError = ""
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{Policy: m.policy, Hostname: m.hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU(), EmulatorAvailable: m.available(), MaxEmulators: m.cfg.MaxEmulators, Templates: append([]Template{}, m.templates...), Instances: append([]Instance{}, m.instances...), Error: m.lastError}
}
func (m *Manager) Allows(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowsLocked(serial)
}

func (m *Manager) allowsLocked(serial string) bool {
	virtual := strings.HasPrefix(serial, "emulator-")
	if m.policy.Mode == "physical" && virtual || m.policy.Mode == "virtual" && !virtual {
		return false
	}
	if len(m.cfg.DeviceAllow) == 0 {
		return !virtual || m.ownsLocked(serial)
	}
	return MatchDeviceAllow(m.cfg.DeviceAllow, serial, virtual && m.ownsLocked(serial))
}

// MatchDeviceAllow applies an explicit device allowlist to an inventory entry.
// The caller supplies its default policy when no entries are configured.
func MatchDeviceAllow(entries []string, serial string, managed bool) bool {
	virtual := strings.HasPrefix(serial, "emulator-")
	for _, entry := range entries {
		if entry == serial || entry == "physical:*" && !virtual || entry == "managed-only" && managed {
			return true
		}
	}
	return false
}

func (m *Manager) PreWarm(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy.PreWarm && m.allowsLocked(serial) && (!strings.HasPrefix(serial, "emulator-") || m.ownsLocked(serial))
}
func (m *Manager) Owns(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ownsLocked(serial)
}

func (m *Manager) ownsLocked(serial string) bool {
	for _, i := range m.instances {
		if i.Serial == serial && i.State != "restarting" {
			return true
		}
	}
	return false
}

// Preserve a failed lease cleanup across backend tunnel reconnections.
func (m *Manager) Quarantine(serial, transportID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quarantined[serial] = transportID
}

func (m *Manager) Quarantined(serial, transportID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, exists := m.quarantined[serial]
	return exists && id == transportID
}

// WipeRequired is keyed by serial so an interrupted wipe cannot be bypassed by
// reconnecting a device with a fresh transport ID or by restarting the agent.
func (m *Manager) WipeRequired(serial string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wipeRequired[serial]
}

func (m *Manager) SetWipeRequired(serial string, required bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]bool, len(m.wipeRequired)+1)
	for device, pending := range m.wipeRequired {
		next[device] = pending
	}
	if required {
		next[serial] = true
	} else {
		delete(next, serial)
	}
	if err := writePrivate(filepath.Join(m.cfg.StateDir, "wipe-required.json"), next); err != nil {
		return err
	}
	m.wipeRequired = next
	return nil
}

func logPanic(operation string) {
	if p := recover(); p != nil {
		slog.Error("agent worker panicked", "operation", operation, "panicType", fmt.Sprintf("%T", p), "stack", string(debug.Stack()))
	}
}
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
	syscall.Flock(int(m.lock.Fd()), syscall.LOCK_UN)
	m.lock.Close()
}

func (m *Manager) run(ctx context.Context) {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		for _, p := range m.processes {
			m.stop(cleanup, p)
		}
		m.updateAwake(cleanup, false)
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		func() { defer logPanic("reconcile machine"); m.reconcile(ctx) }()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-m.wake:
		}
	}
}
func (m *Manager) reconcile(ctx context.Context) {
	m.mu.Lock()
	policy := m.policy
	m.mu.Unlock()
	wanted := make(map[string]bool)
	sum := sha256.Sum256([]byte(m.cfg.ID + "/" + policy.Image))
	prefix := "marionette-" + hex.EncodeToString(sum[:5])
	for i := 0; i < policy.EmulatorCount; i++ {
		wanted[prefix+"-"+strconv.Itoa(i+1)] = true
	}
	for name, p := range m.processes {
		if !wanted[name] {
			m.stop(ctx, p)
			delete(m.processes, name)
		}
	}
	var instances []Instance
	var failures []string
	for i := 0; i < policy.EmulatorCount; i++ {
		if ctx.Err() != nil {
			return
		}
		name := prefix + "-" + strconv.Itoa(i+1)
		p := m.processes[name]
		if p == nil {
			var err error
			p, err = m.start(ctx, name, policy.Image)
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			m.processes[name] = p
		}
		state := "starting"
		select {
		case <-p.done:
			state = "restarting"
			delete(m.processes, name)
		default:
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			data, err := m.adbCommand(probe, "-s", p.serial, "shell", "getprop", "sys.boot_completed").Output()
			cancel()
			if err == nil && strings.TrimSpace(string(data)) == "1" {
				state = "ready"
			}
		}
		instances = append(instances, Instance{Name: name, Serial: p.serial, State: state})
	}
	m.updateAwake(ctx, policy.KeepAwake && policy.Mode != "virtual")
	m.mu.Lock()
	m.instances = instances
	m.lastError = strings.Join(failures, "; ")
	m.mu.Unlock()
}
