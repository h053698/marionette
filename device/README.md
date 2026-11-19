# Android device runner

Original Java implementation for the shell `app_process` environment. Sources use
Java 8 syntax and Android API 34 SDK APIs. The root build tooling produces the dex
JAR; the launch command and complete wire contract are in `../PROTOCOL.md`.

The runner captures the default display into a MediaCodec AVC
input Surface. Encoded dimensions preserve aspect ratio approximately, rounded
down to codec alignment, capped by `--max-size`. Codec capability checks reject
unsupported size/rate combinations explicitly. SPS bytes determine the advertised
RFC 6381 codec string. Both Annex B and four-byte length-prefixed encoder output
are handled; codec metadata must contain SPS and PPS. No fabricated video,
screenrecord process, scrcpy code, Appium, custom IME, or root is used.

## Live preview profiles

The control channel additionally accepts
`{"id":"...","action":"videoProfile","profile":"preview"}` or the exact
profile name `interactive`. Other names and non-string values return `ok:false`.
The two presets are:

| Profile | Maximum edge | Maximum frame rate | Target bit rate |
| --- | ---: | ---: | ---: |
| `preview` | 480 px | 8 fps | 600000 bit/s |
| `interactive` | 1280 px | 30 fps | 4000000 bit/s |

Changing a preset atomically requests recreation of the actual encoder and display
mirror. Persistent control/video sockets stay connected. The encoder publishes new
metadata, actual SPS/PPS and an IDR through the existing bounded fan-out. The
control success response acknowledges the requested preset; it does not promise
that new metadata has already arrived. Unsupported codec sizes/rates still fail
explicitly on the video channel. Repeating the current settings avoids recreation.
Standalone CLI defaults and custom launch arguments remain unchanged until a
named preset is requested.

The host uses preview launch arguments only while admin preview viewers need an
otherwise idle device. `/start` on a valid operator lease selects interactive;
lease release/expiry returns to preview if admin viewers remain. One encoder is
shared per device, so the wall receives interactive quality while an operator is
active, rather than maintaining a second thumbnail encoder. When neither an
active started lease nor admin viewers remain, the host stops its runner.

Admin previews use `GET /api/admin/devices/{serial}/preview` WebSocket binary
messages in the existing video protocol, without claiming or renewing a lease.
The host requires a server-assigned admin identity; incoming preview WebSocket
messages cannot invoke controls. Session-context cancellation closes both preview
and operator video sockets. Admin decoder recovery uses
`POST /api/admin/devices/{serial}/keyframe` to coalesce an IDR request on an
existing runner, returning `{"ok":true}`; it never starts a runner or grants input.
An unknown device returns 404, and no live runner returns 409.
The internal `videoProfile` action is intentionally
not exposed by the HTTP actions allowlist. Host lifecycle tests use fake ADB and
protocol metadata. A subsequent direct API 34 emulator test verified native
preview → interactive → preview transitions, actual decoding and reconnects;
see `PREVIEW_VALIDATION.md`. Physical-device behavior and sustained performance
remain unmeasured; no latency or throughput improvement is claimed.

## Framework paths and limits

`Platform.java` isolates hidden framework reflection:

- `ActivityThread.systemMain/getSystemContext` creates a context attributed to
  the installed `com.android.shell` package.
- API 30–33 first attempts `SurfaceControl.createDisplay`, transaction methods,
  `setDisplaySurface`, `setDisplayProjection`, `setDisplayLayerStack`, and
  `destroyDisplay`. It reads the default display layer stack through reflection.
- API 34, or failure of the legacy path, uses `DisplayManager.createVirtualDisplay`
  with PUBLIC and AUTO_MIRROR flags. This depends on the shell UID having the
  platform capture permission; it is not ordinary application MediaProjection.
  OEM permission restrictions, hidden API changes, and display service policies
  can reject it. Failure produces a video type-4 error and control `info` error.
- Input calls hidden `InputManager.getInstance/injectInputEvent` on API 30–33
  and `InputManagerGlobal` on API 34, waiting for injection completion. Android
  still decides whether shell may inject into the target window.
- Text lazily attaches `UiAutomation` using a hidden connection constructor and
  `connect(FLAG_DONT_SUPPRESS_ACCESSIBILITY_SERVICES)`. It finds the active
  window's input-focused editable node and performs `ACTION_SET_TEXT`, replacing
  the field's full contents with the supplied Unicode string. This is not typing
  or insertion at the cursor. Inaccessible/custom/password fields may reject it;
  rejection is an explicit error. Other accessibility services remain enabled,
  but concurrent automation clients may prevent attachment. Selectors can also
  address editable fields directly by resource ID, exact text, description or
  class. `uiTree` returns JSON/XML with up to 2000 nodes and depth 64; password
  text is masked. `findElement` and `click` use the same live UiAutomation
  connection, with optional waits up to 8000ms and FLAG_REPORT_VIEW_IDS enabled.

The target range is Android 11–14 (API 30–34); other API levels are rejected.
These paths require authorized adb shell access. **Target range is not a claim
of hardware validation.** One official API 34 arm64 emulator was exercised; see
`EMULATOR_VALIDATION.md`. Secure/protected surfaces are subject to Android's
capture restrictions and may appear blank. Audio, external displays, foldable
display switching, and vendor-specific capture workarounds are outside v1.

Rotation, resolution, and density are polled every 250 ms. A change releases and
recreates the mirror, surface, and encoder, clears old codec headers, publishes
new metadata/configuration, and requests an IDR. Input uses current logical
display pixel coordinates, as reported in metadata. Orientation correctness on
each vendor implementation still needs device testing.

## Transport and lifecycle

An abstract LocalServerSocket accepts reconnecting video (byte 1) and control
(byte 2) channels. Commands serialize across clients, including complete swipe
gestures. There are at most 16 handshake/control connections and eight video
writers. Each video writer has an eight-packet bounded queue and socket timeout;
overflow clears dependencies, repeats headers, requests an IDR, and waits for a
keyframe. No socket write runs on the encoder or control execution thread.
Output format changes and reconnects also repeat headers before an IDR. The
queue bound is by packet count; individual packets are limited to 16 MiB.
An idle surface encoder may stop repeating an unchanged display despite its
repeat-frame setting. If a requested keyframe does not arrive within two seconds,
the runner recreates the encoder and real display mirror to obtain fresh input.
After three unsuccessful recreation attempts, capture fails explicitly. This
can introduce roughly a two-second delay when joining a static screen; it never
creates simulated pixels. Frame rate is a ceiling, not a constant-rate promise.

Control JSON lines are bounded to 1 MiB and strictly decoded as UTF-8. Malformed
JSON gets an error response; malformed UTF-8, oversized/truncated lines, and
unknown channel bytes close that connection. Idle control sockets remain open.
Shutdown closes listeners/clients, stops the encoder, releases the mirror and
input surface, and disconnects accessibility. Abrupt process death also releases
Android-owned binder/display resources; SIGKILL cannot execute Java cleanup.

## Validation

On 2026-11-16, all five Java sources compiled successfully using the local JDK,
`javac -encoding UTF-8 -source 8 -target 8 -bootclasspath
.tools/android-sdk/platforms/android-34/android.jar`. D8 from Android build-tools
34.0.0 also converted the compiled classes successfully with `--min-api 30` and
the API 34 library. Generated validation artifacts are under the ignored
`device/.validation/` directory. Compiler output contained only deprecation notes.
The repository build command `bash scripts/build-device.sh` also completed
successfully and produced `build/marionette-device.jar`; JAR inspection confirmed
that it contains `classes.dex`.
Direct runtime validation subsequently passed on one official Android 14/API 34
arm64 emulator, including actual AVC decoding, input, Unicode replacement,
rotation and reconnects. It found and fixed initial accessibility-window timing
and idle keyframe recovery issues. Full results, commands, artifacts, and scope
are in `EMULATOR_VALIDATION.md`. No physical-device validation was performed.
The Go host and Chrome console also passed lease/start/video/Home/release and
Unicode text checks against this emulator; the same report records those results
separately from the direct protocol tests.

Compilation and real-device validation are separate. A successful SDK compile
does not verify shell permissions, hidden signatures on OEM ROMs, display
orientation, actual AVC decoding, accessibility, or input injection. Before
claiming a device supported, check a real stream, portrait/landscape transitions,
reconnections and slow clients, tap/swipe/key, Unicode replacement, and cleanup.
