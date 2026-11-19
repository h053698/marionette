package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"marionette/internal/auth"
	"marionette/internal/backend"
	"marionette/internal/jobqueue"
	"marionette/internal/service"
)

func main() {
	if err := run(); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}
func run() (err error) {
	dataDir := service.Env("MARIONETTE_DATA_DIR", service.Env("MARIONETTE_STATE_DIR", ".marionette"))
	listen := flag.String("listen", service.Env("MARIONETTE_LISTEN", "127.0.0.1:8080"), "backend HTTP listen address")
	tokenFile := flag.String("token-file", service.Env("MARIONETTE_TOKEN_FILE", filepath.Join(dataDir, "tokens.json")), "personal API token hash store")
	userFile := flag.String("user-file", service.Env("MARIONETTE_USER_FILE", filepath.Join(dataDir, "users.json")), "known website user registry (identity and timestamps only)")
	oauthFile := flag.String("oauth-file", service.Env("MARIONETTE_OAUTH_FILE", filepath.Join(dataDir, "oauth.json")), "OAuth client registrations and refresh-token hashes for the hosted MCP endpoint")
	jobFile := flag.String("job-file", service.Env("MARIONETTE_JOB_FILE", filepath.Join(dataDir, "jobs.json")), "durable job store (one backend writer)")
	agentFile := flag.String("agent-file", service.Env("MARIONETTE_AGENT_FILE", filepath.Join(dataDir, "agents.json")), "durable agent registration hash store")
	agentPackages := flag.String("agent-packages", service.Env("MARIONETTE_AGENT_PACKAGES", "build"), "directory containing the public Linux agent packages and checksums")
	installScript := flag.String("install-script", os.Getenv("MARIONETTE_INSTALL_SCRIPT"), "public Ubuntu installer (defaults to scripts/setup-ubuntu-agent.sh alongside the package directory or relative to the working directory)")
	flag.Parse()
	level, format, err := service.ConfigureLogging()
	if err != nil {
		return err
	}
	if err := service.ValidateListen(*listen); err != nil {
		return err
	}
	if info, err := os.Stat(*agentPackages); *agentPackages == "" || err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
		return errors.New("--agent-packages / MARIONETTE_AGENT_PACKAGES must point to a directory")
	}
	if *installScript != "" {
		if info, err := os.Lstat(*installScript); err != nil || !info.Mode().IsRegular() {
			return errors.New("--install-script / MARIONETTE_INSTALL_SCRIPT must point to a regular file")
		}
	}
	queue := jobqueue.FromEnv()
	if err := service.ValidateKafka(queue.Brokers, queue.Topic, queue.TLS, queue.Username, queue.Password); err != nil {
		return err
	}
	jobsEnabled := len(queue.Brokers) > 0
	runnerToken := os.Getenv("MARIONETTE_RUNNER_TOKEN")
	if jobsEnabled && !service.TokenValid(runnerToken) {
		return errors.New("MARIONETTE_RUNNER_TOKEN requires at least 32 bytes and no whitespace when Kafka jobs are enabled")
	}
	leasePolicy, err := service.LeasePolicyEnv()
	if err != nil {
		return err
	}
	files := [][2]string{{"MARIONETTE_TOKEN_FILE", *tokenFile}, {"MARIONETTE_USER_FILE", *userFile}, {"MARIONETTE_OAUTH_FILE", *oauthFile}, {"MARIONETTE_AGENT_FILE", *agentFile}}
	if jobsEnabled {
		if *jobFile == "" {
			return errors.New("--job-file / MARIONETTE_JOB_FILE must not be empty when Kafka jobs are enabled")
		}
		files = append(files, [2]string{"MARIONETTE_JOB_FILE", *jobFile})
	}
	if err := service.ValidateStateFiles(files...); err != nil {
		return err
	}
	var tokens map[string]string
	if raw := os.Getenv("MARIONETTE_AGENT_TOKENS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
			return errors.New("MARIONETTE_AGENT_TOKENS must be a JSON object mapping agent IDs to tokens")
		}
	}
	authConfig := auth.Config{
		PublicOrigin: os.Getenv("MARIONETTE_PUBLIC_URL"),
		IssuerURL:    os.Getenv("MARIONETTE_OIDC_ISSUER"), ClientID: os.Getenv("MARIONETTE_OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("MARIONETTE_OIDC_CLIENT_SECRET"), RedirectURL: os.Getenv("MARIONETTE_OIDC_REDIRECT_URL"),
		AdminEmails: service.List(os.Getenv("MARIONETTE_ADMIN_EMAILS")), AdminSubjects: service.List(os.Getenv("MARIONETTE_ADMIN_SUBJECTS")),
		FrontendOrigins: service.List(os.Getenv("MARIONETTE_FRONTEND_ORIGINS")), TokenFile: *tokenFile, UserFile: *userFile, OAuthFile: *oauthFile,
	}
	for _, setting := range []struct {
		name               string
		target             *time.Duration
		fallback, min, max time.Duration
	}{
		{"MARIONETTE_SESSION_TTL", &authConfig.SessionTTL, auth.DefaultSessionTTL, auth.MinSessionTTL, auth.MaxSessionTTL},
		{"MARIONETTE_OAUTH_ACCESS_TTL", &authConfig.OAuthAccessTTL, auth.DefaultOAuthAccessTTL, time.Second, 24 * time.Hour},
		{"MARIONETTE_OAUTH_REFRESH_TTL", &authConfig.OAuthRefreshTTL, auth.DefaultOAuthRefreshTTL, time.Second, 365 * 24 * time.Hour},
		{"MARIONETTE_OAUTH_CODE_TTL", &authConfig.OAuthCodeTTL, auth.DefaultOAuthCodeTTL, time.Second, time.Hour},
	} {
		*setting.target, err = service.DurationEnv(setting.name, setting.fallback, setting.min, setting.max)
		if err != nil {
			return err
		}
	}
	ctx, finish := service.ShutdownContext()
	defer func() { finish(err) }()
	app, err := backend.NewWithLeasePolicy(tokens, leasePolicy)
	if err != nil {
		return err
	}
	defer app.Close()
	setupCtx, setupCancel := context.WithTimeout(ctx, 15*time.Second)
	authentication, err := auth.New(setupCtx, authConfig)
	setupCancel()
	if err != nil {
		return fmt.Errorf("configure OIDC: %w", err)
	}
	defer authentication.Close()
	app.EnableMembers(authentication.Members)
	if err := app.EnableAgentStore(*agentFile); err != nil {
		return fmt.Errorf("configure agent registrations: %w", err)
	}
	if jobsEnabled {
		if err := app.EnableJobs(queue, *jobFile, runnerToken); err != nil {
			return fmt.Errorf("configure jobs: %w", err)
		}
	}
	closeOnStop := context.AfterFunc(ctx, app.Close)
	defer closeOnStop()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if ctx.Err() != nil || !authentication.Configured() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /internal/agents/{id}/connect", app.Connect)
	mux.HandleFunc("POST /internal/runners/jobs/{id}/{operation}", app.RunnerHTTP)
	backend.MountAgentDownloads(mux, *agentPackages, *installScript)
	mux.Handle("/", authentication.Wrap(app))
	redirect := authConfig.RedirectURL
	if redirect == "" && authentication.Configured() {
		redirect = authentication.Origin() + "/auth/callback"
	}
	service.LogLifecycle("backend configuration", level, format,
		slog.String("listen", *listen), slog.String("public_url", authentication.Origin()),
		slog.Bool("oidc_configured", authentication.Configured()), slog.String("oidc_issuer", authConfig.IssuerURL),
		slog.String("oidc_client_id", authConfig.ClientID), slog.String("oidc_redirect_url", redirect), slog.Any("frontend_origins", authConfig.FrontendOrigins),
		slog.Int("admin_email_count", len(authConfig.AdminEmails)), slog.Int("admin_subject_count", len(authConfig.AdminSubjects)),
		slog.String("data_dir", dataDir), slog.String("token_file", *tokenFile), slog.String("user_file", *userFile),
		slog.String("oauth_file", *oauthFile), slog.String("agent_file", *agentFile), slog.String("job_file", *jobFile),
		slog.Duration("session_ttl", authConfig.SessionTTL), slog.Duration("oauth_access_ttl", authConfig.OAuthAccessTTL),
		slog.Duration("oauth_refresh_ttl", authConfig.OAuthRefreshTTL), slog.Duration("oauth_code_ttl", authConfig.OAuthCodeTTL),
		slog.Duration("lease_default_ttl", leasePolicy.DefaultTTL), slog.Duration("lease_max_ttl", leasePolicy.MaxTTL),
		slog.String("agent_packages", *agentPackages), slog.String("install_script", cmp.Or(*installScript, "auto")),
		slog.Int("preregistered_agents", len(tokens)), slog.Bool("jobs_enabled", jobsEnabled), slog.Any("kafka_brokers", queue.Brokers),
		slog.String("kafka_topic", queue.Topic), slog.Bool("kafka_tls", queue.TLS), slog.Bool("kafka_sasl", queue.Username != ""),
		slog.GroupAttrs("resource_limits", app.ResourceLimitLogAttrs()...))
	return service.Serve(ctx, *listen, mux)
}
