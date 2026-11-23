.PHONY: tools device backend agent frontend agent-package agent-package-linux mcp cli cli-package runner run run-backend run-agent run-web test check

tools:
	python3 scripts/setup-tools.py

device:
	bash scripts/build-device.sh

backend:
	go build -o build/marionette-backend ./cmd/marionette-backend

agent:
	go build -o build/marionette-agent ./cmd/marionette-agent

frontend:
	go build -o build/marionette-web ./cmd/marionette-web

agent-package: device
	bash scripts/package-agent.sh

agent-package-linux: device
	GOOS=linux GOARCH=amd64 bash scripts/package-agent.sh
	GOOS=linux GOARCH=arm64 bash scripts/package-agent.sh

mcp:
	go build -o build/marionette-mcp ./cmd/marionette-mcp

cli:
	go build -o build/marionette ./cmd/marionette

cli-package:
	python3 scripts/package-cli.py --version "$(or $(VERSION),dev)"

runner:
	go build -o build/marionette-runner ./cmd/marionette-runner

run: run-backend

run-backend:
	bash scripts/run.sh backend

run-agent:
	bash scripts/run.sh agent

run-web:
	bash scripts/run.sh web

test:
	go test -race ./...
	node --test web/*.test.mjs

check:
	node --check web/input.mjs
	go vet ./...
	node --check web/app.mjs
	node --check web/video.mjs
	node --check web/apps.mjs
	node --check web/agents.mjs
	node --check web/users.mjs
	node --check web/hover.mjs
	node --check web/tooltips.mjs
	node --check web/tokens.mjs
	node --check web/picker.mjs
	node --check web/files.mjs && node --check web/transfer.mjs && node --check web/assign.mjs && node --check web/dialogs.mjs
	node --check web/config.mjs
	node --check web/config.js
	bash -n scripts/*.sh
	python3 -m py_compile scripts/*.py
