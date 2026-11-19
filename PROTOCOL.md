# Marionette v1 implementation contract

This repository implements its own Android screen encoder and input controller.
Do not vendor, execute, or wrap scrcpy or Appium. Android SDK/platform tools and
Android framework APIs are allowed. Initial target: Android 11–14 (API 30–34),
with version-specific hidden APIs isolated and unsupported devices failing clearly.
No root is required; the device must authorize USB debugging for the host.

## Device process

Build a dex JAR at `build/marionette-device.jar`. Push it to
`/data/local/tmp/marionette-device.jar` and start it with:

```
CLASSPATH=/data/local/tmp/marionette-device.jar app_process / dev.marionette.Main
```

Optional arguments: `--socket <name>` (default `marionette`), `--max-size <pixels>`
(default 1280), `--bit-rate <bits/sec>` (default 4000000), `--max-fps <fps>`
(default 30). Use an abstract Android LocalServerSocket. Logs go to stderr.
Capture/control errors must be returned explicitly; never generate simulated frames.

Each socket connection starts with one byte: `0x01` selects video, `0x02`
selects general RPC, `0x03` selects hover input, and `0x04` selects ordered
touch/key/text input. `0x05` selects the host-only lease cleanup connection.
The input sockets are independent of package/process/tree queries. All use
the same device process. Support reconnects. A new video
connection receives current metadata and codec configuration, then an IDR frame
is requested. Slow video clients must not block all clients or input handling.

## Video wire format

Each packet: `type: uint8`, `payloadLength: uint32 big-endian`, then payload.
Maximum payload length: 16 MiB.

- Type 1: UTF-8 JSON metadata:
  `{"width":720,"height":1280,"displayWidth":1080,"displayHeight":1920,"rotation":0,"codec":"avc1.42E01F"}`.
   Width/height are encoded dimensions; displayWidth/displayHeight are the current
   logical display pixels for input, including rotation and any display-size
   override (not density-independent pixels or encoded video pixels). Codec is an RFC 6381 AVC codec
  string derived from the actual SPS (a baseline example is shown above).
- Type 2: Annex B codec configuration bytes (SPS/PPS).
- Type 3: `timestampUs: uint64 big-endian`, `flags: uint8` (bit 0 = keyframe),
  followed by one complete Annex B H.264 access unit. Minimum payload size is
  10 bytes: nine header bytes and at least one access-unit byte.
- Type 4: UTF-8 JSON error `{"error":"message"}`.

The browser receives the same complete packets as WebSocket binary messages.
It decodes with WebCodecs. For an Annex B keyframe, prepend cached SPS/PPS when
needed. On orientation/size changes, send updated metadata and configuration,
then a keyframe. Bound queues; after dropping dependent frames wait for an IDR.

## Control wire format

UTF-8 newline-delimited JSON, maximum request/response 1 MiB. Each request has
`id` (string), `action` (string), and the fields below. Each response echoes `id`
and contains either `{"ok":true,"result":...}` or
`{"ok":false,"error":"message"}`. Serialize general RPC per device. Input has
separate locks; never queue hover behind a process listing or hierarchy traversal.
Input sockets accept only input actions, and closing one cancels its held pointer.
The host adds a positive `leaseEpoch` to state-changing commands. On channel
`0x05`, `resetInput` with that epoch revokes those commands, cancels active
gestures, and acknowledges only after accepted mutations and pointer cleanup
finish. The host strips client-supplied epochs and does not expose `resetInput`
as an HTTP action.

- `info`: current metadata plus Android SDK/model information if available.
- `tap`: `x`, `y` in current logical display pixels.
- `touch`: realtime socket only, `phase` is `down`, `move`, `up` or `cancel`, with
  `x`, `y`. Moves and end events must belong to the socket that started the touch.
- `hover`: `phase` is `move` or `exit`, with `x`, `y`; native mouse tool/source.
- `swipe`: `x1`, `y1`, `x2`, `y2`, `durationMs` (50–5000).
- `key`: Android `keyCode` integer.
- `text`: `text` string. Unicode may use a focused accessibility node with
  ACTION_SET_TEXT; explicitly report unsupported input rather than silently
  corrupting it. No custom IME is required in v1.
- `uiTree`: `format` is `json` (default) or `xml`; return the current accessibility
  hierarchy, bounded to 2000 nodes and depth 64. Password text is masked.
- `findElement`: `selector:{resourceId,text,description,className,index}` with
  exact string predicates and optional `waitMs` (0–8000). Return element bounds
  and accessibility attributes. Resource IDs require FLAG_REPORT_VIEW_IDS.
- `click`: the same selector; use the node click action or its visible center.
  `text` also accepts this selector to replace a specific editable field.
- `keyframe`: request an encoder IDR.
- `videoProfile`: internal host command, `profile` is exactly `preview` or
  `interactive`. Reconfigure capture on the existing sockets and send new
  metadata/configuration followed by an IDR. Preview targets max edge 480,
  8fps, 600000 bit/s; interactive targets 1280, 30fps, 4000000 bit/s. The
  command acknowledgement accepts a change; video metadata and frames confirm
  completion. This action is not exposed through the HTTP actions endpoint.

## Host HTTP API

Run `cmd/marionette-backend`, `cmd/marionette-agent` and `cmd/marionette-web`
separately. The agent connects outbound to the backend over an authenticated
WebSocket/yamux tunnel and serves no public TCP port. The backend validates
identity and injects a private identity header on agent requests. Public device
IDs contain the agent ID and encoded raw serial. `cmd/marionette` is the remote
user CLI. The frontend serves `web/` and SPA routes `/admin`, `/devices`, `/login`.
The backend validates its configured public origin (by default the OIDC callback
origin); it does not trust forwarded Host headers.

### Public Ubuntu installer downloads

The backend serves these assets without a session cookie or other credentials.
Both `GET` and `HEAD` are supported. Responses use `Cache-Control: no-cache`
and `X-Content-Type-Options: nosniff`. Missing assets return `404`.

- `/install/ubuntu.sh` (`text/x-shellscript; charset=utf-8`) looks for
  `scripts/setup-ubuntu-agent.sh` under the parent of the package directory, then
  relative to the working directory. Override the path with `--install-script`
  or `MARIONETTE_INSTALL_SCRIPT`.
- `/downloads/marionette-agent-linux-amd64.tar.gz` and
  `/downloads/marionette-agent-linux-arm64.tar.gz` use `application/gzip`.
- `/downloads/marionette-agent-linux-SHA256SUMS` uses `text/plain; charset=utf-8`.

The three download files come from `--agent-packages` or
`MARIONETTE_AGENT_PACKAGES` (default `build`). Only the listed filenames are served.

### Authentication

Configure `MARIONETTE_OIDC_ISSUER`, `MARIONETTE_OIDC_CLIENT_ID`,
`MARIONETTE_OIDC_CLIENT_SECRET`, `MARIONETTE_OIDC_REDIRECT_URL`. The CLI requires
all four or none. With no configuration, the login UI remains available but
device APIs return 503. With configuration, unauthenticated device APIs return
401. Register a confidential OIDC client supporting Authorization Code + PKCE
S256 and the exact `/auth/callback` URL. HTTPS is required outside loopback.

- `GET /api/session`: public `{authenticated,configured,loginURL}`; authenticated
  responses also include `user:{subject,email,name,role}`, `csrfToken`, `expiresAt`.
- `GET /auth/login`: begin a browser-bound, one-use state/nonce/PKCE transaction.
- `GET /auth/callback`: exchange code, verify the signed ID token, issue an opaque
  HttpOnly SameSite=Lax session cookie, redirect to the application.
- `POST /auth/logout`: invalidate the local session and existing session streams.

Sessions are in memory with an eight-hour default lifetime. HTTPS uses Secure
cookies. Every state-changing cookie-authenticated request, including logout and
lease operations, requires `X-CSRF-Token` from `/api/session`. WebSockets use the
session cookie and allowed-origin checks. Session expiry/logout cancels existing
stream contexts. A separately configured HTTPS frontend uses SameSite=None for
the session cookie and credentialed CORS for its allowed origin.

REST clients may instead authenticate with `Authorization: Bearer mnt_...`.
These requests do not require CSRF. Explicit invalid credentials never fall back
to a session cookie. Personal tokens always have role `user`, including tokens
created by admins. Token expiry/revocation cancels in-flight token requests.
Token hashes and owner metadata persist in the host's `--token-file` (default
`.marionette/tokens.json`), with 0600 atomic file replacement. Raw tokens are only
returned on creation. Logout does not revoke personal tokens.

- `GET /api/tokens`: browser session only; current user's metadata, no hashes or secrets.
- `POST /api/tokens`: browser session + CSRF; `{name,expiresInDays}` →
  `{id,name,createdAt,expiresAt,token}`. Default 30 days, range 1–90, max 20 active
  tokens per user. Token names contain 1–100 UTF-8 bytes.
- `DELETE /api/tokens/{id}`: browser session + CSRF; revoke only own tokens.

Bearer credentials cannot call token management or `/auth/*`. Token revocation
does not release the user's lease; release it first or wait for its TTL.

`MARIONETTE_ADMIN_SUBJECTS` grants admin by exact subject. Alternatively,
`MARIONETTE_ADMIN_EMAILS` grants admin only if the provider attests
`email_verified: true`. Other authenticated users have role `user`. The admin
role permits preview and ending or taking over another user's lease. Control
still requires a valid subject-bound lease; takeover issues a new one.

An authenticated account may own at most one device across all agents. This
includes browser, personal tokens, CLI, MCP and jobs. Members see only their
assigned device in `GET /api/devices`; unassigned members receive an empty list
and use `POST /api/device/occupy` to request one. Requests for another device's
screen, control, apps, processes or capture fail with 403. A second distinct
device claim fails with 409. Agent assignment discovery fails closed when an
online agent cannot confirm ownership. If a tunnel drops, the backend keeps
the last known device ID, lease ID and expiry for that account and agent. The
account cannot claim a different device until the remembered lease expires or
the reconnected agent confirms the old assignment has ended. An ambiguous
mutation result can also block another claim for up to the configured maximum
lease duration. A backend restart resets both in-memory guards. Administrators
retain full monitoring access.

`GET /api/devices/{serial}/control` upgrades to a separate input WebSocket. The
first JSON message must be `{type:"auth",token:"lease token"}` within five seconds;
the token is not placed in the URL. After `{type:"ready"}`, send input actions
with unique IDs and receive the native ACKs. Frames are limited to 64 KiB.
Adjacent move samples may be replaced by the newest position; down/up/key/text
order is preserved. The browser sends moves on animation frames without waiting
for each ACK. Commands are never replayed after reconnect. Lease loss closes
with 1008; native runtime restart closes with 1011 and allows reconnect.

### Hosted MCP and OAuth 2.1

`/mcp` serves Streamable HTTP MCP. It accepts only `Authorization: Bearer` with an
OAuth access token (`mat_`, default 1 hour) or a personal token (`mnt_`). Other credentials receive
`401` with `WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource"`.
The request `Host` must match the host and port of the configured public origin.
An `Origin` header, when present, must match the public origin or a configured
frontend. Allowed origins can read `WWW-Authenticate` and `Mcp-Session-Id` on
actual responses. Callers run as role `user`. Tools check the credential on each
call and device requests are canceled on credential expiry or revocation. MCP
sessions expire after 30 minutes idle and are limited to 8 per user and 1,024
per server. Revoking the grant closes its sessions. Refreshed OAuth access tokens
can resume sessions for the same grant.

- `GET /.well-known/oauth-protected-resource[/mcp]`, `GET /.well-known/oauth-authorization-server[/mcp]`: public metadata (RFC 9728, RFC 8414), S256 only, public clients only, `iss` response parameter.
- `POST /oauth/register` accepts up to 8 HTTPS, loopback HTTP, or private-use redirect URIs and 4 KiB of serialized metadata per client. The cap is 512 clients. Registrations with active grants, pending codes, or pending consent are protected from eviction. The rate limit uses the direct peer IP, with a burst of 8 and a refill of one request per 30 seconds.
- `GET|POST /oauth/authorize` uses a browser session and sends signed-out users to `/auth/login?next=`. GET shows a consent page with a browser-session-bound, single-use consent ID. POST submits the ID, CSRF field, and `decision=allow|deny`. Approval issues a single-use code valid for 5 minutes by default. Errors before a consent decision display locally.
- `POST /oauth/token` accepts `authorization_code` with `code_verifier` and rotates `refresh_token`. Reuse of the latest rotated token within its 10-minute detection window revokes the family. `resource`, when sent, must be `<origin>/mcp`.
- `POST /oauth/revoke` revokes an OAuth access token or, when given a recognized refresh token, its family (RFC 7009).
- `GET /api/oauth/grants`, `DELETE /api/oauth/grants/{id}`: the signed-in user's connected apps; browser session only, not usable with bearer tokens.

Registrations and refresh-token hashes persist to `--oauth-file` (default `.marionette/oauth.json`, 0600). Reads and writes enforce the same 8 MiB store and 4,096 refresh-record limits. Issuance and rotation return `500 server_error` on a storage failure. Capacity errors return `503 temporarily_unavailable`. Revocation takes effect in memory immediately and failed writes are retried by the background cleaner. Access tokens and codes live in memory.

### Device APIs

- `GET /api/status`: administrators receive agent health details; ordinary members
  receive only `{connected:true}`.
- `GET /api/devices`: `{"devices":[{"serial","model","state","occupied","owner","expiresAt","leaseId"}]}`.
  Members receive only their own assignment (`mine:true`). Administrator inventory
  includes unauthorized/offline devices; actions require `state=device`.
  An unassigned device being cleaned reports `state=cleaning`. A failed, timed
  out, or interrupted cleanup reports `state=cleaning_failed`. Both states
  reject claims with 409. The supervised agent retains unfinished cleanup
  across restarts and transport reconnects.
  macOS/Linux agents also report USB-detected Android devices as `state=usb`
  when ADB does not list them. USB presence does not attest authorization or lock
  state. Previously discovered devices remain offline in the agent's inventory
  while its current connection is active; they are not persisted across restarts.
  Inventory follows the agent's device allowlist and operating mode. The default
  for the supervised agent includes physical devices and the agent's own
  emulators. A standalone host without a machine manager uses its full ADB
  inventory by default and still honors an explicit `MARIONETTE_DEVICE_ALLOW`.
   Occupied devices include a public `leaseId`, which is not a control credential.
- `GET /api/pool` returns available options under `virtual.devices` and
  `physical.devices`. Each option's `id` is an opaque selection ID tied to the
  signed-in account and valid for up to 10 minutes. A backend restart invalidates
  existing selection IDs. Pass it as `deviceId` to `POST /api/device/occupy` or
  `POST /api/devices/occupy`. Administrator sessions and personal bearer clients
  such as the CLI can still pass a stable device ID.
  Each group reports optional aggregate `cleaning` and `cleaningFailed` counts.
  Those devices are excluded from `total`, `available`, and `devices`.
- `POST /api/devices/{serial}/lease`: `{ttlSeconds}` → `{id,token,owner,expiresAt}`.
  Owner is derived from the authenticated identity and cannot be spoofed by the
  request body. Bind the lease to OIDC subject as well as its token. TTL default
  300s, range 30–3600. The same account can resume its current lease (HTTP 200).
  Supplying `leaseId` makes resumption conditional on that exact current lease;
  stale, revoked or expired IDs return 409 and never allocate a replacement.
  Browser reload and detail-dialog closure retain the lease. On reload the UI
  verifies the authenticated account's inventory, resumes the existing ID and
  renews it; only non-secret view identifiers are kept in sessionStorage.
  Setting `resume:false` requires a fresh lease and returns 409 if occupied; the
  allocator and queued jobs always use this to prevent sharing a racing claim.
- `POST /api/devices/{serial}/attach`: `{code}` exchanges a one-time handoff code
  for the same account's new lease. The backend reads and validates lease and
  attach requests before serializing ownership changes for the account. It
  allows 64 KiB for lease JSON and 4 KiB for attach JSON, with a five-second read
  deadline. The account lock and agent RPC share a 15-second timeout.
- `PUT /api/devices/{serial}/lease`: renew; header `X-Lease-Token`.
- `DELETE /api/devices/{serial}/lease`: release with `X-Lease-Token`. Without
  options, returns 200 `{"ok":true}` and keeps apps and files. Send optional JSON
  `{"wipe":true}` (with `Content-Type: application/json`) or `?wipe=1` to revoke
  the lease and start Android cleanup. Returns 202 `{"released":true,"cleaning":true}`.
  A repeat of the most recent release on the same agent connection with the same
  subject and token returns the original result without starting another wipe.
  Cleanup uninstalls Android user 0 third-party apps except those on the agent's
  keep list and empties `/sdcard`, preserving `/sdcard/Android` and existing
  standard folder shells. System account logins and settings can remain.
  Expiry and administrator disconnection use the usual release without a wipe.
- `POST /api/admin/devices/{serial}/cleanup`: administrator session + CSRF.
  Retries cleanup for an online `cleaning_failed` device, returning 202
  `{"cleaning":true}`.
- `POST /api/admin/devices/{serial}/restore`: administrator session + CSRF.
  Returns 200 `{"restored":true}` after clearing the failed cleanup state without
  wiping. Normal eligibility rules apply. Both endpoints return 409 if the
  device is not in `cleaning_failed`.
- `DELETE /api/admin/devices/{serial}/lease`: administrator session + CSRF;
  `{leaseId}` disconnects the reviewed occupant and frees the device.
- `POST /api/admin/devices/{serial}/lease`: administrator session + CSRF;
  `{leaseId,ttlSeconds}` atomically disconnects the reviewed occupant and issues
  a fresh administrator-bound lease, HTTP 201 `{id,token,owner,expiresAt}`.
  TTL has the same default and range as ordinary claims. The administrator then
  calls `start` using the new lease token. Both admin operations return 409 if
  occupancy has changed since the supplied `leaseId` was observed. They require
  no previous occupant's token. Personal bearer tokens cannot use these routes.
  Old tokens immediately lose renew/action/screenshot/video access; existing
  video connections close with code 1008. Control sockets close before cleanup.
  The agent waits up to five seconds for driver input cleanup and rotation
  restoration before another lease can be granted. If cleanup fails, the same
  ADB transport is ineligible for assignment until it reconnects with a new ID
  or an operator restarts the agent after resolving the problem.
- `POST /api/devices/{serial}/start`: deploy/start runner in interactive profile;
  header `X-Lease-Token`.
  Return `{"ok":true}` (metadata may arrive via video or info action).
- `POST /api/devices/{serial}/actions`: control fields above without required `id`;
  header `X-Lease-Token`. Return the device response. Support `keyframe` too.
- `POST /api/actions/{action}` uses `deviceId` from the caller's assignment and
  `X-Lease-Token`. For members, another account's device and an unknown device
  both return HTTP 403 with `{"error":"device not available"}`.
- `GET /api/devices/{serial}/screenshot`: header `X-Lease-Token`; return a PNG at
  physical display resolution with `Content-Type: image/png` and `Cache-Control:
  no-store`. Requires a live subject-bound lease; does not require a video viewer
  or running driver. Capture is bounded to 10 seconds and a 20 MiB PNG response.
- `GET /api/devices/{serial}/video?token=...`: binary WebSocket. Validate same-origin
  browser requests. Expired/released leases must lose access on existing streams,
  not only on new connections. Allow up to 4 streams per lease, 32 per device,
  and 128 per agent. The device and agent limits include previews. Send a ping
  every 20 seconds and require a pong within a 45-second read window.
- `GET /api/admin/devices/{serial}/preview`: administrator-only read-only binary
   WebSocket. Does not claim or renew a lease; bounded to 32 viewers per device.
   The backend counts video and preview WebSockets until disconnect. Before an
   upgrade it returns HTTP 429 with code `video_limit` if a cap is full.
   Defaults are 128 streams globally (`MARIONETTE_VIDEO_MAX_GLOBAL`) and 4 per
   account or lease (`MARIONETTE_VIDEO_MAX_PER_ACCOUNT`).
- `POST /api/admin/devices/{serial}/keyframe`: administrator-only coalesced IDR
  request for an existing runner, also requiring CSRF. Does not launch a runner.

One runtime/encoder is shared by preview and control for a device. Active
interactive use selects the interactive profile for all viewers of that device;
otherwise previews use the preview profile. Releasing/expiring a lease closes
its control streams and downgrades the runtime if previews remain. Stop the
runtime when neither interactive use nor preview viewers remain. Device
disconnection and server shutdown close both kinds of stream.

The file list returns at most 5,000 entries and reads at most 2 MiB of ADB
output. It marks an incomplete list with `truncated: true`. File upload,
download, and APK install share per-lease and agent-wide concurrency limits.
The agent reserves the declared APK size against a host-wide staging budget
before reading the body. Overflow of the live transfer budget returns 429.
Lease revocation cancels transfers, including blocked body reads. File uploads
stage under an exclusive temporary name in the destination directory and use
a no-clobber move unless the request authorizes overwrite.

HTTP errors: `{"error":"message"}` with suitable 4xx/5xx status. Do not expose
arbitrary adb shell execution endpoints. Always use argument arrays for processes.
Use unique socket names per host runtime, `adb forward tcp:0 localabstract:<name>`,
track and remove only the forward/process created by this host. One persistent
control connection per runner, one video reader with per-viewer bounded fan-out.
Cache metadata/config, request a keyframe for a new viewer, and join only at a
keyframe. On shutdown, close streams/connections, remove forwards, and stop only
the runner owned by this host. The device slot and lease pin the ADB transport ID
from `adb devices -l`. A disconnection or transport change invalidates the lease
and stops the old runtime even when the serial is unchanged. Lease-scoped ADB
commands and direct/relayed ADB services select that transport explicitly.

## Queue-backed jobs

- `POST /api/jobs` accepts `selection`, `steps` and optional `timeoutSeconds`.
  It returns HTTP 202 and a job record. A serialized runner claim over 900 KiB
  returns HTTP 413 with code `job_payload_too_large`.
- A job owner or administrator can use `GET /api/jobs/{id}` and
  `GET /api/jobs/{id}/artifacts/{index}`. `DELETE /api/jobs/{id}` cancels an active
  job and returns HTTP 200 with the canceled record. For a terminal job it
  deletes the record and its artifacts and returns HTTP 204. Once deleted or
  pruned, the job and artifacts return HTTP 404.
- The backend prunes only terminal records. It removes expired records on a
  timer and evicts the oldest eligible records early under store or artifact
  quota pressure. `MARIONETTE_JOB_RETENTION` defaults to `168h`.
  `MARIONETTE_JOB_ARTIFACT_MAX_BYTES_PER_ACCOUNT` defaults to `268435456` bytes
  (256 MiB). If space for a screenshot is still unavailable after eviction,
  the job fails with `job_artifact_limit`.
- Failed and canceled job records include a stable `errorCode` and a generic
  Korean `error` message. Execution and persistence errors returned by the job
  API have `code` and `error` fields. The backend logs diagnostic details with
  the job ID where available.

## Build targets

The device sources are Java 8 syntax compiled against Android API 34, then
converted to DEX with minimum API 30. The host module is `marionette` and requires
Go 1.26 or later. Browser modules use native WebCodecs, Canvas, and WebSocket.
Real hardware validation is recorded separately from builds and host tests.


## Direct ADB (WebRTC)

`POST /api/devices/{serial}/direct` with `X-Lease-Token` and body `{"sdp": "<offer>"}` returns `{"sdp": "<answer>"}`. The offer must be at most 64 KiB and include ICE candidates (non-trickle). The agent accepts up to 4 peers per device. Each peer has a lifetime budget of 64 incoming data channels, including rejected or failed channels, and closes if the budget is exceeded. Clients open ordered, reliable channels labelled `adb`. Each channel starts with the same JSON handshake as `/adb` (`{"service": "watch" | <device service>}` → `{"ok": true, "features": ...}` or `{"error": ...}`), then carries raw service bytes. The peer closes when the authorizing lease ends, the connection fails, or no usable channel opens within 30 seconds. `reverse:` and host services are rejected exactly as on the WebSocket relay. ICE servers come from `--ice` / `MARIONETTE_ICE_SERVERS` on both sides; `none` uses host candidates only.
