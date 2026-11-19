package main

import (
	"flag"
	"log/slog"
	"os"

	"marionette/internal/frontend"
	"marionette/internal/service"
)

func main() {
	if err := run(); err != nil {
		slog.Error("frontend stopped", "error", err)
		os.Exit(1)
	}
}

func run() (err error) {
	listen := flag.String("listen", service.Env("MARIONETTE_LISTEN", "127.0.0.1:3000"), "frontend HTTP listen address")
	dir := flag.String("web-dir", service.Env("MARIONETTE_WEB_DIR", "web"), "browser static files")
	apiURL := flag.String("api-url", os.Getenv("MARIONETTE_API_URL"), "backend origin; empty uses the page origin")
	flag.Parse()
	level, format, err := service.ConfigureLogging()
	if err != nil {
		return err
	}
	if err := service.ValidateListen(*listen); err != nil {
		return err
	}
	app, err := frontend.New(*dir, *apiURL)
	if err != nil {
		return err
	}
	ctx, finish := service.ShutdownContext()
	defer func() { finish(err) }()
	service.LogLifecycle("web configuration", level, format, slog.String("listen", *listen), slog.String("web_dir", *dir), slog.String("api_url", *apiURL))
	return service.Serve(ctx, *listen, app)
}
