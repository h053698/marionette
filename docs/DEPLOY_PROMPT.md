# 배포 담당 에이전트용 프롬프트

아래 블록 전체를 배포를 맡을 에이전트에게 그대로 전달합니다. `<…>`로 표시한 값은 전달하기 전에 채웁니다. 비밀값(OIDC client secret, 에이전트 토큰 등)은 프롬프트에 붙이지 말고 배포 호스트에서 직접 입력하게 합니다.

---

```text
You are deploying Marionette, a self-hosted Android device farm, to production. Work carefully, verify every step, and stop to ask before any action that is hard to reverse.

## What you are deploying
- Repo: https://github.com/h053698/marionette (branch main). Clone it on the deployment host.
- Server side runs on ONE Linux host with Docker Compose: Caddy (public HTTPS, ports 80/443) → marionette-backend (:8080 in the container network) + marionette-web (:3000). Optional profiles: kafka and runner (job queue).
- Device side ("agents") runs on separate Ubuntu PCs that have USB phones or KVM emulators. They connect OUTBOUND to the backend over HTTPS/WSS. No inbound ports are needed on agent PCs.
- The authoritative guides are in the repo. Read them first and follow them over this prompt if they disagree:
  - docs/DEPLOY.md: Docker Compose server deployment (main guide)
  - docs/CONFIG.md: every environment variable, its default and its precedence
  - docs/UBUNTU_AGENT.md and docs/AGENT_INSTALL.md: agent PC setup (one-command installer)
  - docs/API_MCP.md and docs/AUTOMATION.md: user-facing API, MCP (OAuth), CLI and ADB
  - PROTOCOL.md: wire protocol and security model

## Inputs (filled in by the requester)
- Public hostname: <devices.example.com>
- Deployment host: <ssh user@host>, Linux amd64 or arm64 with Docker Engine and Compose v2.30+
- Company OIDC issuer URL: <https://sso.example.com/realms/team>
- OIDC client ID: <marionette>. The client secret is provided out of band; ask for it and never echo it.
- Admin emails (verified email claims) and/or admin OIDC subjects: <admin@company.com>
- Agent PCs to connect (IDs and mode physical/emulator): <ubuntu-01 physical, ubuntu-02 emulator>
- Enable the Kafka job queue? <yes/no>

## Hard rules
- Keep exactly ONE backend replica. State is single-writer JSON in the backend_data volume (/data/state: tokens.json, users.json, agents.json, oauth.json, jobs.json, artifacts/).
- Never run `docker compose down -v` on production. Never delete the backend_data, caddy_data or kafka_data volumes.
- Secrets live only in deploy/docker/backend.env and deploy/docker/jobs.env on the host, chmod 600, in raw KEY=value form (no quotes, do not `source` them). Do not commit them, paste them into chat or logs, or put them in shell history (prefer an editor).
- The OIDC provider must restrict who can log in. Any account that can log in becomes a Marionette user.
- Do not modify application source code. If something is broken, stop and report the exact error with file and line.
- Ask before opening firewall ports other than 80/443, changing DNS, or restarting anything that already serves users.

## Steps
1. Preflight on the deployment host: `docker version`, `docker compose version` (v2.30+), free disk space (at least 10 GB), and the clock (NTP). Confirm DNS A/AAAA for the hostname points to this host and that TCP 80/443 reach it (Caddy issues the certificate via ACME). Clone the repo and record the commit hash you deploy.
2. OIDC: register (or verify) a confidential web client whose redirect URI is EXACTLY https://<host>/auth/callback.
3. Configure:
   export MARIONETTE_DOMAIN=<host>; unset MARIONETTE_API_URL
   cp -n deploy/docker/backend.env.example deploy/docker/backend.env && chmod 600 deploy/docker/backend.env
   Set MARIONETTE_OIDC_ISSUER, MARIONETTE_OIDC_CLIENT_ID, MARIONETTE_OIDC_CLIENT_SECRET, MARIONETTE_PUBLIC_URL=https://<host>, MARIONETTE_OIDC_REDIRECT_URL=https://<host>/auth/callback, MARIONETTE_ADMIN_EMAILS and/or MARIONETTE_ADMIN_SUBJECTS. Leave MARIONETTE_FRONTEND_ORIGINS empty (single origin). The backend refuses to start on a public HTTPS URL without complete OIDC settings and at least one admin; that is intended.
   Optional tuning (see docs/CONFIG.md): MARIONETTE_VIDEO_MAX_GLOBAL, MARIONETTE_VIDEO_MAX_PER_ACCOUNT, MARIONETTE_JOB_RETENTION, MARIONETTE_JOB_ARTIFACT_MAX_BYTES_PER_ACCOUNT, lease TTL limits.
4. Both Dockerfiles build the Linux agent packages and SHA-256 manifest from the same checkout and include them in `/agent-packages`. Remove any old host mount at that path so the container serves its packaged files. The build needs access to Docker Hub, Go module dependencies, and Google's Android SDK repository.
5. Start: `docker compose config -q` → `docker compose up -d --build` → `docker compose ps` (all healthy) → `docker compose logs --tail=100 backend web proxy`. The backend logs its effective non-secret configuration at startup; check public_url, oidc_configured=true and admin counts.
6. Verify the server:
   - `curl -fsS https://<host>/api/session` returns "configured":true
   - `curl -fsSI` returns 200 for `/install/ubuntu.sh`, `/downloads/marionette-agent-linux-SHA256SUMS`, and both Linux `.tar.gz` downloads
   - Open https://<host> in a browser and log in with an admin account. The admin menu (에이전트 운영, 사용자) must be visible. Log in with a normal account in a separate browser profile: it must see only its own assignment ("1대 중 0대 사용 중").
   - `curl -s -o /dev/null -w '%{http_code}' -X POST https://<host>/mcp` returns 401 with a WWW-Authenticate header that points to /.well-known/oauth-protected-resource
   - The certificate is valid and HTTP redirects to HTTPS
7. Connect agent PCs: in the dashboard, open 에이전트 운영 → 에이전트 연결 and register each PC ID. Copy the shown one-line Ubuntu command and run it on that PC as a normal sudo-capable user (not root). Add `-- --mode emulator` for KVM emulator PCs (amd64 only). The token is shown once; keep it out of chat. Then check on the PC: `systemctl --user status marionette-agent`, `loginctl show-user $USER -p Linger` (must be yes), and for USB phones that `adb devices` lists each phone as `device` after accepting the RSA prompt once on the phone. In the dashboard, the agent must be online, show CPU/RAM/disk, and its devices must appear in 기기 모니터링. If the agent should use unmanaged emulators, set MARIONETTE_DEVICE_ALLOW (see docs/CONFIG.md); by default only physical devices and emulators the agent created are used.
8. Functional smoke test as a normal user:
   - 기기 배정받기 → choose 가상머신 or 실물 → live screen → tap, swipe and type text
   - App management, file manager (upload/download), rotate
   - Release with "정리하고 반납" → the device shows "정리 중" and then becomes available again
   - Try a second assignment while holding one: it must be rejected with "이미 기기를 하나 사용 중이에요"
   - As admin: take over a member's device and disconnect it (popups)
   - API/MCP dialog: create a PAT and revoke it. Optionally connect an MCP client to https://<host>/mcp via OAuth.
9. Optional Kafka and runners: follow docs/DEPLOY.md §4 (jobs.env with MARIONETTE_RUNNER_TOKEN from `openssl rand -hex 32`, create the topic, then `--profile kafka --profile runner`). Submit examples/android-job.json with the CLI (`marionette job submit --file …`, then `job wait`) and confirm that it succeeds.
10. Operations:
   - Set up a backup of the backend_data volume as in docs/DEPLOY.md §5 (stop the backend, tar /data/state, start it again). Also back up caddy_data, and kafka_data if used. Do one test backup.
   - Restarts end browser sessions; schedule them.
   - Logs are Docker json-file, 10 MB × 3 per container.
   - Upgrade with `git pull` + `docker compose up -d --build`.

## Known limitations (report them, do not try to fix them)
- "정리하고 반납" removes user-installed apps and /sdcard files. It is not a factory reset; system account logins can remain. TTL expiry, logout and admin disconnect do not wipe.
- Direct WebRTC ADB needs UDP. Without it, the CLI falls back to the backend relay. Configure TURN via MARIONETTE_ICE_SERVERS if needed.
- Not yet verified in production: real certificate issuance, company SSO, physical phones on Ubuntu, KVM emulator boot on a real Ubuntu desktop, Android Studio GUI, and Kafka retention under load.

## Report back
The deployed commit hash; the URL; the `docker compose ps` output; the results of each verification step (pass/fail, with the exact error for failures); the agents connected and their devices; the backup location; and every deviation from this plan, with the reason.
```
