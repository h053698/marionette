# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.11-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY client/ ./client/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    install -d -m 0700 /runtime-data/state && \
    mkdir /out /agent-packages && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -ldflags='-s -w' -o /out/ \
      ./cmd/marionette-backend ./cmd/marionette-web ./cmd/marionette-runner

FROM alpine:3.23
# Prepare directories and the CA bundle on BUILDPLATFORM so cross-builds need no emulation.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=10001:10001 /runtime-data/ /data/
COPY --from=build /agent-packages/ /agent-packages/

WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY web/ /app/web/
COPY scripts/setup-ubuntu-agent.sh /app/scripts/setup-ubuntu-agent.sh

USER 10001:10001
EXPOSE 8080 3000
CMD ["marionette-backend", "--listen=0.0.0.0:8080", "--token-file=/data/state/tokens.json", "--user-file=/data/state/users.json", "--agent-file=/data/state/agents.json", "--oauth-file=/data/state/oauth.json", "--job-file=/data/state/jobs.json", "--agent-packages=/agent-packages", "--install-script=/app/scripts/setup-ubuntu-agent.sh"]
