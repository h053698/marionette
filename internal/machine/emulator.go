package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type process struct {
	name, serial string
	cmd          *exec.Cmd
	done         chan struct{}
}

func (m *Manager) environment() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ANDROID_HOME", "ANDROID_SDK_ROOT", "ANDROID_AVD_HOME", "ANDROID_EMULATOR_HOME", "JAVA_HOME", "MARIONETTE_AGENT_TOKEN":
		default:
			env = append(env, entry)
		}
	}
	env = append(env, "ANDROID_HOME="+m.cfg.SDKRoot, "ANDROID_SDK_ROOT="+m.cfg.SDKRoot, "ANDROID_AVD_HOME="+filepath.Join(m.cfg.StateDir, "avd"), "ANDROID_EMULATOR_HOME="+filepath.Join(m.cfg.StateDir, "emulator"))
	if m.javaHome != "" {
		env = append(env, "JAVA_HOME="+m.javaHome)
	}
	// ANDROID_USER_HOME and ADB_VENDOR_KEYS are deliberately inherited: keep the
	// computer's existing ADB identity when starting its emulator processes.
	return env
}

func (m *Manager) adbCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.cfg.ADB, args...)
	cmd.Env = m.environment()
	return cmd
}

func (m *Manager) start(ctx context.Context, name, image string) (*process, error) {
	for _, path := range []string{"avd", "emulator", "logs"} {
		if err := os.MkdirAll(filepath.Join(m.cfg.StateDir, path), 0700); err != nil {
			return nil, err
		}
	}
	avd := filepath.Join(m.cfg.StateDir, "avd", name+".avd")
	if _, err := os.Stat(filepath.Join(avd, "config.ini")); os.IsNotExist(err) {
		createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		command := exec.CommandContext(createCtx, m.avdmanager, "create", "avd", "-n", name, "-k", image, "-p", avd, "-d", "pixel")
		command.Env = m.environment()
		command.Stdin = strings.NewReader("no\n")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			message := strings.TrimSpace(string(output))
			if len(message) > 700 {
				message = message[len(message)-700:]
			}
			return nil, fmt.Errorf("could not create emulator: %s", message)
		}
		file, err := os.OpenFile(filepath.Join(avd, "config.ini"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, err = file.WriteString("\nhw.lcd.width=720\nhw.lcd.height=1280\nhw.lcd.density=320\nhw.ramSize=1536\nhw.keyboard=yes\ndisk.dataPartition.size=2G\n")
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	// A recorded console port lets an agent adopt its own still-running AVD
	// after an abrupt restart without touching another emulator.
	record := filepath.Join(m.cfg.StateDir, "avd", name+".port")
	port := 0
	if raw, err := os.ReadFile(record); err == nil {
		previous, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		if previous >= 5600 && previous <= 5680 && previous%2 == 0 {
			serial := "emulator-" + strconv.Itoa(previous)
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			data, err := m.adbCommand(probe, "-s", serial, "emu", "avd", "name").Output()
			cancel()
			if err == nil && strings.Split(strings.TrimSpace(string(data)), "\n")[0] == name {
				p := &process{name: name, serial: serial, done: make(chan struct{})}
				go m.watch(ctx, p)
				return p, nil
			}
		}
	}
	for candidate := 5600; candidate <= 5680; candidate += 2 {
		first, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err != nil {
			continue
		}
		second, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", candidate+1))
		first.Close()
		if err != nil {
			continue
		}
		second.Close()
		port = candidate
		break
	}
	if port == 0 {
		return nil, errors.New("no free emulator console port in 5600..5680")
	}
	if err := os.WriteFile(record, []byte(strconv.Itoa(port)), 0600); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(m.cfg.StateDir, "logs", name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	command := exec.Command(m.emulator, "-avd", name, "-port", strconv.Itoa(port), "-no-window", "-no-audio", "-no-snapshot", "-no-boot-anim", "-no-metrics", "-gpu", "swiftshader", "-memory", "1536", "-cores", "2")
	command.Env = m.environment()
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("start emulator: %w", err)
	}
	p := &process{name: name, serial: "emulator-" + strconv.Itoa(port), cmd: command, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer log.Close()
		defer logPanic("wait for emulator")
		_ = command.Wait()
	}()
	return p, nil
}
func (m *Manager) watch(ctx context.Context, p *process) {
	defer close(p.done)
	defer logPanic("watch emulator")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			data, err := m.adbCommand(probe, "-s", p.serial, "emu", "avd", "name").Output()
			cancel()
			if err != nil || strings.Split(strings.TrimSpace(string(data)), "\n")[0] != p.name {
				return
			}
		}
	}
}
func (m *Manager) stop(ctx context.Context, p *process) {
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	data, err := m.adbCommand(probe, "-s", p.serial, "emu", "avd", "name").Output()
	cancel()
	if err == nil && strings.Split(strings.TrimSpace(string(data)), "\n")[0] == p.name {
		stop, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = m.adbCommand(stop, "-s", p.serial, "emu", "kill").Run()
		cancel()
	}
	if p.cmd != nil {
		select {
		case <-p.done:
			return
		default:
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			_ = p.cmd.Process.Kill()
		case <-ctx.Done():
			_ = p.cmd.Process.Kill()
		}
	}
}

func (m *Manager) loadAwake() error {
	raw, err := os.ReadFile(filepath.Join(m.cfg.StateDir, "awake.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var awake map[string]*int
	if len(raw) > 64<<10 || json.Unmarshal(raw, &awake) != nil || awake == nil {
		return errors.New("invalid awake settings store")
	}
	validated := make(map[string]int, len(awake))
	for serial, value := range awake {
		if serial == "" || strings.ContainsAny(serial, "/\\\x00\r\n") || value == nil || *value < 0 || *value > 15 {
			return errors.New("invalid awake settings store")
		}
		validated[serial] = *value
	}
	m.awake = validated
	return nil
}
func (m *Manager) updateAwake(ctx context.Context, enabled bool) {
	if m.awakeDisabled {
		return
	}
	if enabled {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		data, err := m.adbCommand(probe, "devices", "-l").Output()
		cancel()
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || fields[1] != "device" || strings.HasPrefix(fields[0], "emulator-") || strings.ContainsAny(fields[0], "/\\\x00\r\n") {
					continue
				}
				serial := fields[0]
				if !m.Allows(serial) || slices.Contains(fields[2:], "device:marionette_remote") {
					continue
				}
				var transport string
				for _, field := range fields[2:] {
					if id, ok := strings.CutPrefix(field, "transport_id:"); ok {
						if value, err := strconv.ParseUint(id, 10, 64); err == nil && value != 0 {
							transport = strconv.FormatUint(value, 10)
						}
					}
				}
				if transport == "" {
					continue
				}
				if _, exists := m.awake[serial]; !exists {
					get, cancel := context.WithTimeout(ctx, 2*time.Second)
					raw, err := m.adbCommand(get, "-t", transport, "shell", "settings", "get", "global", "stay_on_while_plugged_in").Output()
					cancel()
					original, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
					if err != nil || parseErr != nil || original < 0 || original > 15 {
						continue
					}
					m.awake[serial] = original
					if writePrivate(filepath.Join(m.cfg.StateDir, "awake.json"), m.awake) != nil {
						delete(m.awake, serial)
						continue
					}
				}
				set, cancel := context.WithTimeout(ctx, 2*time.Second)
				_ = m.adbCommand(set, "-t", transport, "shell", "svc", "power", "stayon", "true").Run()
				cancel()
			}
		}
	} else {
		for serial, original := range m.awake {
			if strings.ContainsAny(serial, "/\\\x00\r\n") {
				continue
			}
			get, cancel := context.WithTimeout(ctx, 2*time.Second)
			raw, err := m.adbCommand(get, "-s", serial, "shell", "settings", "get", "global", "stay_on_while_plugged_in").Output()
			cancel()
			if err != nil {
				continue
			}
			value := strings.TrimSpace(string(raw))
			if value == "7" || value == "15" {
				set, cancel := context.WithTimeout(ctx, 2*time.Second)
				err = m.adbCommand(set, "-s", serial, "shell", "settings", "put", "global", "stay_on_while_plugged_in", strconv.Itoa(original)).Run()
				cancel()
				if err != nil {
					continue
				}
			}
			delete(m.awake, serial)
		}
		_ = writePrivate(filepath.Join(m.cfg.StateDir, "awake.json"), m.awake)
	}
}
