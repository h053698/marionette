# OIDC and administrator dashboard validation

Date: 2026-11-16. Tests ran locally on macOS arm64. This record separates
layout fixtures, real device capture and authentication-provider configuration.

## Automated checks

- Go race tests cover OIDC state, browser binding, PKCE, nonce, signed claims,
  admin roles, CSRF, session revocation, lease races and subject binding,
  preview permissions, profile transitions, cleanup and bounded video fan-out.
- Node tests cover packet bounds, coordinates, Annex B keyframe configuration,
  asynchronous decoder configuration, rotation, queue overload and resource cleanup.
- Java 8/API 34 compilation and D8 conversion produce the actual device DEX JAR.
- `go vet` and JavaScript/Python syntax checks complete the repository checks.

Commands: `make device host test check`.

## Browser: dense grid and roles

The ignored QA harness `build/qa/oidc-farm.go` uses the production auth middleware
and host server. Its local provider issues real RS256 ID tokens and checks PKCE
S256. The browser follows the normal SSO redirect and callback; no production
login bypass or demo account was added.

For the layout run, the harness supplies 27 online fixture devices plus offline
and unauthorized entries. Each fixture replays an IDR and SPS/PPS extracted
from a previously captured real Android H.264 stream. All fixtures show the same
recorded launcher at 720×1280 and approximately 1fps. They do not implement the
native preview quality profiles and do not establish 27-device throughput.

Observed through the actual browser UI:

- Administrator login rendered 29 inventory tiles and 27 decoded/live screens.
- At 1440px, the grid had nine columns and no horizontal overflow.
- Pause closed every fixture video connection: `activeVideoConnections: 0`.
- Resume, search, read-only detail, claim, Home, release and close succeeded.
- At 390×844, the grid had two columns and no horizontal overflow.
- Logout returned to the login view and `/api/session` reported unauthenticated.
- A member login rendered inventory with zero live overview screens. A direct
  GET of the administrator preview route returned 403.

Artifacts in `build/qa/`: `oidc-login.png`, `oidc-admin-wall.png`,
`oidc-admin-mobile.png`, `oidc-device-detail.png`, `oidc-member.png` and the
corresponding `oidc-dashboard.js.log` / `oidc-member.js.log`.

## Browser: actual Android capture through OIDC and Go

The same QA server ran with `--real`, replacing fixture ADB with the actual ADB
executable and the official Android 14/API 34 arm64 emulator `emulator-5580`.
The emulator had two CPUs and 1536 MiB RAM. Capture ran as Android shell without
root and used the repository's Java MediaCodec driver.

The real browser completed this sequence in one run:

1. SSO login through the local OIDC provider.
2. Administrator tile decoded the actual 270×480 preview.
3. Read-only detail retained 270×480 with controls disabled.
4. Claim/start switched the decoded frame to 720×1280 and enabled controls.
5. Home completed through the production HTTP control endpoint.
6. Release restored 270×480 and disabled the Home control.
7. Close and logout returned to the login view with an unauthenticated session.

Artifacts: `build/qa/oidc-real.js.log`, `oidc-real-preview.png`,
`oidc-real-control.png`. The JSON markers report `270×480 → 720×1280 → 270×480`.
Screenshots were inspected and showed actual launcher content inside the console.
Native persistent-socket reconfiguration and actual IDR NAL decoding were also
tested separately; see [native preview validation](../device/PREVIEW_VALIDATION.md).

## Limits

The local test provider validates integration, not an organization's actual
SSO configuration. No user-supplied issuer/client credentials were available,
so connecting a production provider remains a deployment setup step. Copy
`.env.example`, register the exact callback and enter real credentials.

The 8/30fps and 600kbps/4Mbps values are encoder targets, not measured sustained
performance. No physical-device, OEM, Android 11–13, long-running-load or
end-to-end input latency claim is made. The prior drag-on-release input behavior
is retained; hover and live pointer streaming are outside this change.

All QA providers, fixture servers and the task's emulator are stopped after
validation. Fixture code, credentials and screenshots stay under ignored `build/`.
