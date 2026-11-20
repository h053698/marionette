package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"marionette/internal/agent"
	"marionette/internal/direct"
	"marionette/internal/host"
	"marionette/internal/machine"
	"marionette/internal/service"
)

func main() {
	if err := run(); err != nil {
		slog.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run() (err error) {
	backendURL := flag.String("backend", os.Getenv("MARIONETTE_BACKEND_URL"), "relay origin (HTTPS outside loopback)")
	id := flag.String("id", os.Getenv("MARIONETTE_AGENT_ID"), "registered agent ID")
	adb := flag.String("adb", service.Env("MARIONETTE_ADB", "adb"), "adb executable path")
	jar := flag.String("device-jar", service.Env("MARIONETTE_DEVICE_JAR", defaultDeviceJar()), "Android device dex JAR")
	tags := flag.String("tags", os.Getenv("MARIONETTE_DEVICE_TAGS"), "comma-separated tags advertised by this agent's devices")
	platform := flag.String("platform", service.Env("MARIONETTE_PLATFORM", "android"), "device platform (android)")
	sdkRoot := flag.String("android-sdk", os.Getenv("MARIONETTE_ANDROID_SDK"), "Android SDK root; otherwise inferred from the adb path or SDK environment")
	stateDir := flag.String("state-dir", os.Getenv("MARIONETTE_STATE_DIR"), "private operation policy and managed emulator directory")
	ice := flag.String("ice", os.Getenv("MARIONETTE_ICE_SERVERS"), "comma-separated STUN/TURN URLs for direct ADB, or none (default: public STUN)")
	maxEmulators := flag.Int("max-emulators", 2, "maximum managed emulators this PC permits (0 disables creation)")
	flag.Parse()
	level, format, err := service.ConfigureLogging()
	if err != nil {
		return err
	}
	specified := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { specified[f.Name] = true })
	if raw := os.Getenv("MARIONETTE_MAX_EMULATORS"); !specified["max-emulators"] && raw != "" {
		*maxEmulators, err = strconv.Atoi(raw)
		if err != nil {
			return errors.New("MARIONETTE_MAX_EMULATORS must be an integer between 0 and 32")
		}
	}
	if *maxEmulators < 0 || *maxEmulators > 32 {
		return errors.New("--max-emulators / MARIONETTE_MAX_EMULATORS must be between 0 and 32")
	}
	if *platform != "android" {
		return errors.New("--platform / MARIONETTE_PLATFORM must be android")
	}
	if !agent.ValidID(*id) {
		return errors.New("--id / MARIONETTE_AGENT_ID requires a valid registered agent ID")
	}
	token := os.Getenv("MARIONETTE_AGENT_TOKEN")
	if !service.TokenValid(token) {
		return errors.New("MARIONETTE_AGENT_TOKEN requires at least 32 bytes and no whitespace")
	}
	origin, err := service.Origin(*backendURL)
	if err != nil {
		return fmt.Errorf("--backend / MARIONETTE_BACKEND_URL %w", err)
	}
	iceServers, err := configuredICE(*ice)
	if err != nil {
		return err
	}
	leases, err := service.LeasePolicyEnv()
	if err != nil {
		return err
	}
	upload, err := transferLimit("MARIONETTE_MAX_UPLOAD_BYTES", host.DefaultMaxUploadBytes)
	if err != nil {
		return err
	}
	download, err := transferLimit("MARIONETTE_MAX_DOWNLOAD_BYTES", host.DefaultMaxDownloadBytes)
	if err != nil {
		return err
	}
	staging, err := transferLimit("MARIONETTE_MAX_STAGING_BYTES", host.DefaultMaxStagingBytes)
	if err != nil {
		return err
	}
	leaseTransfers, err := transferCount("MARIONETTE_MAX_LEASE_TRANSFERS", host.DefaultMaxLeaseTransfers)
	if err != nil {
		return err
	}
	agentTransfers, err := transferCount("MARIONETTE_MAX_AGENT_TRANSFERS", host.DefaultMaxAgentTransfers)
	if err != nil {
		return err
	}
	discovery, err := service.DurationEnv("MARIONETTE_DISCOVERY_INTERVAL", host.DefaultDiscoveryInterval, time.Second, time.Minute)
	if err != nil {
		return err
	}
	wipeTimeout, err := service.DurationEnv("MARIONETTE_WIPE_TIMEOUT", host.DefaultWipeTimeout, time.Second, time.Hour)
	if err != nil {
		return err
	}
	wipeKeepPackages := service.List(os.Getenv("MARIONETTE_WIPE_KEEP_PACKAGES"))
	if specified["adb"] || os.Getenv("MARIONETTE_ADB") != "" {
		if _, err := exec.LookPath(*adb); err != nil {
			return errors.New("--adb / MARIONETTE_ADB must name an executable ADB program")
		}
	}
	if specified["device-jar"] || os.Getenv("MARIONETTE_DEVICE_JAR") != "" {
		if info, err := os.Stat(*jar); err != nil || !info.Mode().IsRegular() {
			return errors.New("--device-jar / MARIONETTE_DEVICE_JAR must point to a regular JAR file")
		}
	}
	if *sdkRoot != "" {
		if info, err := os.Stat(*sdkRoot); err != nil || !info.IsDir() {
			return errors.New("--android-sdk / MARIONETTE_ANDROID_SDK must point to an SDK directory")
		}
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(".marionette", "agent-"+*id)
	}
	deviceTags := service.List(*tags)
	deviceAllow := service.List(os.Getenv("MARIONETTE_DEVICE_ALLOW"))
	ctx, finish := service.ShutdownContext()
	defer func() { finish(err) }()
	manager, err := machine.New(ctx, machine.Config{ID: *id, ADB: *adb, SDKRoot: *sdkRoot, StateDir: *stateDir, MaxEmulators: *maxEmulators,
		DeviceAllow: deviceAllow})
	if err != nil {
		return fmt.Errorf("configure agent state and policy: %w", err)
	}
	defer manager.Close()
	var iceCount int
	for _, server := range iceServers {
		iceCount += len(server.URLs)
	}
	service.LogLifecycle("agent configuration", level, format,
		slog.String("backend_url", origin), slog.String("agent_id", *id), slog.String("platform", *platform),
		slog.String("adb", *adb), slog.String("device_jar", *jar), slog.String("state_dir", *stateDir),
		slog.String("android_sdk", cmp.Or(*sdkRoot, os.Getenv("ANDROID_SDK_ROOT"), os.Getenv("ANDROID_HOME"), "auto")), slog.Any("device_tags", deviceTags),
		slog.Int("max_emulators", *maxEmulators), slog.Int("ice_server_count", iceCount), slog.Duration("discovery_interval", discovery),
		slog.Duration("lease_default_ttl", leases.DefaultTTL), slog.Duration("lease_max_ttl", leases.MaxTTL),
		slog.Int64("max_upload_bytes", upload), slog.Int64("max_download_bytes", download), slog.Int("device_allow_count", len(deviceAllow)),
		slog.Int("max_lease_transfers", leaseTransfers), slog.Int("max_agent_transfers", agentTransfers), slog.Int64("max_staging_bytes", staging))
	return agent.Run(ctx, agent.Config{BackendURL: origin, ID: *id, Token: token}, func(ctx context.Context, listener net.Listener) error {
		cfg := host.Config{ADB: host.ExecADB{Path: *adb}, DeviceJar: *jar, Tags: deviceTags, Machine: manager,
			ICEServers: iceServers, Leases: leases, MaxUploadBytes: upload, MaxDownloadBytes: download, DiscoveryInterval: discovery,
			MaxLeaseTransfers: leaseTransfers, MaxAgentTransfers: agentTransfers, MaxStagingBytes: staging,
			WipeTimeout: wipeTimeout, WipeKeepPackages: wipeKeepPackages}
		app := host.New(cfg)
		app.Start(ctx)
		defer app.Close()
		server := &http.Server{Handler: agent.WithIdentity(app), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		closeOnStop := context.AfterFunc(ctx, func() { server.Close() })
		defer closeOnStop()
		defer server.Close()
		return server.Serve(listener)
	})
}

func transferLimit(name string, fallback int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 && value <= 1<<40 {
		return value, nil
	}
	return 0, fmt.Errorf("%s must be an integer from 1 through 1099511627776 bytes", name)
}

func transferCount(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	if value, err := strconv.Atoi(raw); err == nil && value >= 1 && value <= 128 {
		return value, nil
	}
	return 0, fmt.Errorf("%s must be an integer from 1 through 128", name)
}

func configuredICE(raw string) ([]webrtc.ICEServer, error) {
	servers := direct.ICEServers(raw)
	username, password := os.Getenv("MARIONETTE_ICE_USERNAME"), os.Getenv("MARIONETTE_ICE_PASSWORD")
	turn := false
	for i := range servers {
		for _, entry := range servers[i].URLs {
			u, err := url.Parse(entry)
			if err != nil || u.Opaque == "" || strings.ContainsAny(entry, "/@#") {
				return nil, errors.New("MARIONETTE_ICE_SERVERS must contain STUN/TURN URLs such as stun:stun.example.com:3478, or none")
			}
			if _, err := service.Origin("https://" + u.Opaque); err != nil {
				return nil, errors.New("MARIONETTE_ICE_SERVERS contains an invalid server host or port")
			}
			switch u.Scheme {
			case "stun", "stuns":
				if u.ForceQuery || u.RawQuery != "" {
					return nil, errors.New("MARIONETTE_ICE_SERVERS STUN URLs must not include a query")
				}
			case "turn", "turns":
				if u.ForceQuery || u.RawQuery != "" && u.RawQuery != "transport=udp" && u.RawQuery != "transport=tcp" {
					return nil, errors.New("MARIONETTE_ICE_SERVERS TURN URLs only support transport=udp or transport=tcp")
				}
				if strings.TrimSpace(username) == "" || password == "" {
					return nil, errors.New("TURN requires MARIONETTE_ICE_USERNAME and MARIONETTE_ICE_PASSWORD")
				}
				servers[i].Username, servers[i].Credential = username, password
				turn = true
			default:
				return nil, errors.New("MARIONETTE_ICE_SERVERS requires stun, stuns, turn or turns URLs, or none")
			}
		}
	}
	if !turn && (username != "" || password != "") {
		return nil, errors.New("MARIONETTE_ICE_USERNAME and MARIONETTE_ICE_PASSWORD require a TURN URL in MARIONETTE_ICE_SERVERS")
	}
	return servers, nil
}

func defaultDeviceJar() string {
	if executable, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(executable), "marionette-device.jar")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return "build/marionette-device.jar"
}
