package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ShutdownContext logs the service's lifecycle around signal-triggered cleanup.
// Defer finish after creating the context and before deferring resource cleanup.
func ShutdownContext() (ctx context.Context, finish func(error)) {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case sig := <-signals:
			cancel(shutdownSignal{sig})
			LogLifecycle("shutdown requested", slog.String("signal", sig.String()))
		case <-ctx.Done():
		}
	}()
	return ctx, func(err error) {
		signal.Stop(signals)
		cancel(context.Canceled)
		<-stopped
		if sig, ok := context.Cause(ctx).(shutdownSignal); ok {
			if err != nil {
				slog.Error("shutdown failed", "signal", sig.String(), "error", err)
			} else {
				LogLifecycle("shutdown complete", slog.String("signal", sig.String()))
			}
		}
	}
}

type shutdownSignal struct{ os.Signal }

func (s shutdownSignal) Error() string { return s.String() }

// LogLifecycle records infrequent startup/shutdown messages at every log level.
func LogLifecycle(message string, attrs ...slog.Attr) {
	record := slog.NewRecord(time.Now(), slog.LevelInfo, message, 0)
	record.AddAttrs(attrs...)
	_ = slog.Default().Handler().Handle(context.Background(), record)
}

func Env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func List(raw string) []string {
	var values []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func DurationEnv(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum || value%time.Second != 0 {
		return 0, fmt.Errorf("%s must be a whole-second duration between %s and %s", name, minimum, maximum)
	}
	return value, nil
}

func ConfigureLogging() (slog.Attr, slog.Attr, error) {
	levelName := strings.ToLower(Env("MARIONETTE_LOG_LEVEL", "info"))
	var level slog.Level
	switch levelName {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return slog.Attr{}, slog.Attr{}, errors.New("MARIONETTE_LOG_LEVEL must be debug, info, warn or error")
	}
	format := strings.ToLower(Env("MARIONETTE_LOG_FORMAT", "text"))
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch format {
	case "text":
		handler = slog.NewTextHandler(os.Stderr, options)
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, options)
	default:
		return slog.Attr{}, slog.Attr{}, errors.New("MARIONETTE_LOG_FORMAT must be text or json")
	}
	slog.SetDefault(slog.New(handler))
	return slog.String("log_level", levelName), slog.String("log_format", format), nil
}

func listenPort(value string, minimum uint64) bool {
	if value == "" || strings.ContainsFunc(value, func(r rune) bool { return r < '0' || r > '9' }) {
		return false
	}
	port, err := strconv.ParseUint(value, 10, 16)
	return err == nil && port >= minimum
}

func ValidateListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !listenPort(port, 0) || strings.ContainsAny(host, " /\\\t\r\n") {
		return errors.New("--listen / MARIONETTE_LISTEN must be host:port with port 0–65535")
	}
	return nil
}

func Loopback(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

// HTTPURL validates an externally configured URL without echoing malformed values.
func HTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if port := u.Port(); port != "" && !listenPort(port, 1) {
		return nil, errors.New("URL port must be 1–65535")
	}
	if bracketed := strings.HasPrefix(u.Host, "["); (bracketed || strings.Contains(u.Hostname(), ":")) &&
		(!bracketed || !strings.Contains(u.Hostname(), ":") || net.ParseIP(u.Hostname()) == nil) {
		return nil, errors.New("invalid URL host")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && Loopback(u.Hostname())) {
		return nil, errors.New("URL requires HTTPS except on loopback")
	}
	return u, nil
}

func Origin(raw string) (string, error) {
	u, err := HTTPURL(raw)
	if err != nil {
		return "", err
	}
	if u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return "", errors.New("origin must not include a path")
	}
	return u.Scheme + "://" + u.Host, nil
}

func TokenValid(value string) bool {
	return len(value) >= 32 && !strings.ContainsAny(value, " \t\r\n")
}

var kafkaTopic = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)

func ValidateKafka(brokers []string, topic string, tls bool, username, password string) error {
	for _, broker := range brokers {
		host := broker
		if strings.Contains(broker, ":") && net.ParseIP(broker) == nil {
			var port string
			var err error
			host, port, err = net.SplitHostPort(broker)
			if err != nil || !listenPort(port, 1) {
				return errors.New("MARIONETTE_KAFKA_BROKERS requires a comma-separated list of hosts with optional valid ports")
			}
		}
		if host == "" || strings.ContainsAny(host, " /\\\t\r\n@?#") {
			return errors.New("MARIONETTE_KAFKA_BROKERS requires a comma-separated list of broker hosts")
		}
	}
	if !kafkaTopic.MatchString(topic) || topic == "." || topic == ".." {
		return errors.New("MARIONETTE_KAFKA_TOPIC must be a valid Kafka topic name (1–249 ASCII letters, digits, dots, hyphens or underscores)")
	}
	if raw := os.Getenv("MARIONETTE_KAFKA_TLS"); raw != "" && raw != "true" && raw != "false" {
		return errors.New("MARIONETTE_KAFKA_TLS must be true or false")
	}
	if (username != "" || password != "") && (!tls || username == "" || password == "") {
		return errors.New("Kafka SASL/PLAIN requires MARIONETTE_KAFKA_TLS=true and both MARIONETTE_KAFKA_USERNAME and MARIONETTE_KAFKA_PASSWORD")
	}
	return nil
}

// ValidateStateFiles checks path collisions and existing file types without writing state.
func ValidateStateFiles(files ...[2]string) error {
	seen := make(map[string]string)
	for _, file := range files {
		name, path := file[0], file[1]
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("%s must be a valid file path", name)
		}
		parent := filepath.Dir(abs)
		for {
			if info, err := os.Stat(parent); err == nil && info.IsDir() {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s requires an accessible parent directory", name)
			}
			parent = filepath.Dir(parent)
		}
		if info, err := os.Stat(abs); err == nil && !info.Mode().IsRegular() || err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s must point to a regular file or a path for a new file", name)
		}
		if existing := seen[abs]; existing != "" {
			return fmt.Errorf("%s and %s must use different paths", name, existing)
		}
		seen[abs] = name
	}
	return nil
}

const (
	MinLeaseTTL     = 30 * time.Second
	DefaultLeaseTTL = 5 * time.Minute
	DefaultMaxLease = time.Hour
	LeaseTTLCeiling = 24 * time.Hour
)

type LeasePolicy struct {
	DefaultTTL, MaxTTL time.Duration
}

func LeasePolicyEnv() (LeasePolicy, error) {
	var policy LeasePolicy
	var err error
	policy.DefaultTTL, err = DurationEnv("MARIONETTE_LEASE_DEFAULT_TTL", DefaultLeaseTTL, MinLeaseTTL, LeaseTTLCeiling)
	if err != nil {
		return policy, err
	}
	policy.MaxTTL, err = DurationEnv("MARIONETTE_LEASE_MAX_TTL", DefaultMaxLease, MinLeaseTTL, LeaseTTLCeiling)
	if err != nil {
		return policy, err
	}
	if policy.DefaultTTL > policy.MaxTTL {
		return policy, errors.New("MARIONETTE_LEASE_DEFAULT_TTL must not exceed MARIONETTE_LEASE_MAX_TTL")
	}
	return policy, nil
}

func (p LeasePolicy) Duration(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return p.DefaultTTL, nil
	}
	if seconds < int(MinLeaseTTL/time.Second) || seconds > int(p.MaxTTL/time.Second) {
		return 0, fmt.Errorf("ttlSeconds must be between %d and %d", int(MinLeaseTTL/time.Second), int(p.MaxTTL/time.Second))
	}
	return time.Duration(seconds) * time.Second, nil
}

func Serve(ctx context.Context, address string, handler http.Handler) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	LogLifecycle("HTTP server listening", slog.String("address", listener.Addr().String()))
	select {
	case err := <-result:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("HTTP serve: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		if err := <-result; err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("HTTP serve: %w", err)
		}
		return nil
	}
}
