# Android 14 emulator validation

Date: 2026-11-16. This is an **emulator result**, not physical hardware validation
and not a claim of Android 11–13 or OEM compatibility. The direct runner tests
spoke the device protocol through adb forwarding. A separate integration pass
used the Go host and Chrome browser console, as recorded below.

## Environment and official downloads

- Apple Silicon arm64 macOS 27.0.1; host RAM 16 GiB; Hypervisor.framework available.
- Android Emulator 37.2.13.0, build 16740729, stable channel.
- Google APIs Android 14/API 34 arm64-v8a system image, revision 14.
- Guest fingerprint:
  `google/sdk_gphone64_arm64/emu64a:14/UE1A.230829.050/12077443:userdebug/dev-keys`.
- Driver ran as `uid=2000(shell)`, SELinux `u:r:shell:s0`, without root.
- 720×1280 display, density 320. Two guest CPUs (`0-1`). Requested 1536 MiB;
  guest `/proc/meminfo` reported `MemTotal: 1511076 kB` after kernel reservations.

Official metadata was fetched from:

- `https://dl.google.com/android/repository/repository2-3.xml`
- `https://dl.google.com/android/repository/sys-img/google_apis/sys-img2-3.xml`

Both archive byte counts and the checksums published in that XML were verified
before extraction:

| Archive | Bytes | SHA-1 |
| --- | ---: | --- |
| `emulator-darwin_aarch64-16740729.zip` | 416113219 | `6c5e61b873e11c7d1d08683bea089f762baee070` |
| `arm64-v8a-34_r14.zip` | 1610393229 | `2fe8b46d419a3400e30f31b0152b241b50c8b99f` |

Total downloaded: 2,026,506,448 bytes. Archives and repository XML are in
`.tools/downloads/`; extracted tools/images are in `.tools/android-sdk/`.
No global SDK or user's existing AVD was changed. Emulator-generated temporary
crash/runtime files may use the emulator's own OS temporary directory.

## Launch and direct testing

Ignored test scaffolding is in `build/emulator/`: `start.py`, `launch-driver.py`,
`probe.py`, `idle-probe.py`, and `stop-driver.py`. The AVD configuration is under
`build/emulator/avd/marionette-api34.avd/`. The launcher sets task-local
`ANDROID_AVD_HOME`, `ANDROID_USER_HOME`, and `ANDROID_EMULATOR_HOME`, without
changing `HOME`.

The final emulator launch used the local binary and these flags:

```text
-avd marionette-api34 -port 5580
-sysdir <repo>/.tools/android-sdk/system-images/android-34/google_apis/arm64-v8a
-memory 1536 -cores 2 -no-window -no-audio -no-snapshot
-gpu swiftshader -no-boot-anim -no-metrics -qemu -m 1536
```

The emulator initially raised `-memory 1536` to 2560 MB automatically. The final
`-qemu -m 1536` override was verified in the booted guest. A restart port check
was corrected to detect an active listener instead of treating TCP TIME_WAIT
as another running emulator.

`bash scripts/build-device.sh` compiled Java 8 against API 34 and converted it
with D8 `--min-api 30`, successfully producing `build/marionette-device.jar`.
The test pushed a separate `/data/local/tmp/marionette-direct-test.jar`, started
`app_process / dev.marionette.Main --socket marionette_direct_test`, and used
`adb -s emulator-5580 forward tcp:0 localabstract:marionette_direct_test`.

## Results

| Check | Observed result |
| --- | --- |
| Initial stream | 40 AVC access units, two keyframes, 32 bytes of SPS/PPS, 715080 frame bytes; 720×1280, `avc1.42C029` |
| Reconnection | Metadata and SPS/PPS followed by an IDR; ten frames captured in the initial reconnect test |
| Actual decoding | FFmpeg decoded pixels from the AVC stream; Constrained Baseline, yuv420p, BT.709. Decoded PNGs visibly showed the launcher and Settings search |
| `info` | Correct dimensions, rotation, codec, SDK 34 and `sdk_gphone64_arm64` model |
| Tap, swipe, key | Native injection returned success. A driver tap opened Settings search; Home returned to the launcher |
| Unicode text | `Marionette 한글 日本語 café 😀` replaced the focused stock Settings search field and was visibly correct in decoded landscape video |
| Rotation | Updated 1280×720/rotation 1 metadata and configuration followed by IDR; final build also returned to 720×1280/rotation 0 with IDR |
| Idle keyframes | Final build served 12 requested keyframes; one idle-recovery request took 2313 ms. A subsequent reconnect received an IDR |
| Non-reading peer | A second video socket read nothing while the regular client received 783892 frame bytes and 12 keyframes; keyframe and final `info` control responses took 0–2 ms |
| Explicit errors | Negative coordinates, unsupported `uiTree`, and text with no focused editable node all returned `ok:false` with actionable errors |
| Cleanup | Owned driver stopped, its individual forward removed, remote test JAR/PID files removed, and display dump showed only the built-in display |

The non-reading-peer check demonstrates that another client and controls remain
responsive in that scenario; it is not exhaustive queue-overflow or long-term
load testing. No secure-screen, external-display, OEM, or physical-device tests
were performed. Browser/Go integration is recorded separately below.

## Runtime fixes and test limitations

1. A first accessibility request could see a null active window just after
   UiAutomation connected. Text entry now waits up to 1500 ms for an initial
   accessible window, while preserving explicit errors for unsupported fields.
   A freshly launched rebuilt driver passed the first text request.
2. The emulator surface encoder stopped producing repeated frames on static
   content. Waiting for an arbitrary 200-frame count therefore timed out, and
   repeated IDR requests could stall awaiting fresh input. Capture now recreates
   the actual encoder/display mirror after a two-second unmet keyframe request,
   with bounded retries. The final idle test exercised this recovery successfully.
3. Home is portrait-locked on this image, so a rotation-setting test on Home
   correctly left its logical display in portrait and timed out waiting for
   landscape. The final round-trip test used rotation-capable Settings and passed.
4. Raw Annex B captures have no container timestamps. The media tool's direct
   seek-to-zero image extraction failed; a lossless timestamp-generating MP4
   remux allowed successful image extraction. Any 25 fps inferred by that remux
   is a tooling default, not a measured driver frame rate.

## Go host and browser integration

The real Go server ran on `http://127.0.0.1:8080`, using the repository's ADB and
built device JAR. Chrome accessed the normal console through Aside. No device
responses or video frames were mocked.

- Selected `emulator-5580`, acquired a lease, started the driver, and displayed
  actual H.264 video through WebCodecs. The decoded canvas was 720×1280 with
  nonuniform pixel content.
- Used the console's Home button and observed the launcher on the guest. Released
  the lease; `/api/devices` reported `occupied:false`, and no ADB forwards remained.
- Submitted `Browser 한글 日本語 café 😀` through the console's text form into the
  focused Settings search field. Both a guest screenshot and a fresh browser
  connection's decoded canvas visibly contained the exact text.
- Confirmed no horizontal overflow at desktop 1440×1000 and mobile 390×844
  viewports. Mobile text submission and release also passed.
- Rebuilt the device JAR and reran `make device test check`: Java/D8 compilation,
  Go race tests, five browser protocol tests, Go vet, and syntax checks passed.

Live rotation through the browser was not tested; the direct runner rotation
round trip above passed. These checks do not establish physical-device support,
multi-device throughput, sustained frame rate, or long-running reliability.

Local integration artifacts are under the ignored `build/qa/` directory:

- `browser-smoke.py`, `text-mobile-smoke.py`: browser interaction scripts.
- `decoded-frame.png`: a fresh browser's actual decoded frame containing Unicode.
- `device-after-browser-text.png`: guest screenshot confirming the text result.
- `stream.png`: desktop console with the real guest screen.
- `desktop.png`, `mobile.png`: empty-state desktop/mobile layout checks.

An immediate screenshot after the text command completed still showed a previous
video frame. The text result was therefore checked on the guest and in a fresh
browser stream; command completion does not mean the browser has rendered the
resulting frame yet.

## Direct test artifacts and cleanup

All paths below are relative to the repository:

- `build/emulator/download-manifest.json`: official URLs, sizes and checksums.
- `build/emulator/emulator.log`, `launch-command.txt`, `emulator.pid`: emulator state.
- `build/emulator/video-results.json`, `controls-results.json`, `text-results.json`,
  `rotation-results.json`: direct protocol results from the first successful run.
  The controls result was later refreshed against the rebuilt driver in landscape.
- `build/emulator/final-results.json`: final idle recovery, explicit errors,
  non-reading peer and reconnect results.
- `build/emulator/final-rotation-results.json`: final build's successful rotation round trip.
- `build/emulator/portrait.mp4`, `portrait.png`: original successful portrait capture,
  losslessly remuxed and decoded. The raw `portrait.h264` was later overwritten
  by an idle diagnostic; use the MP4/PNG for the original capture.
- `build/emulator/landscape-steady.h264`, `landscape.mp4`, `landscape.png`: decoded
  landscape capture visibly containing the Unicode text.
- `build/emulator/avc-probe.json`, `landscape-probe.json`: FFmpeg stream/pixel analysis.
- `build/emulator/driver-idle-tested.log`: capture log including idle recovery.
- `build/emulator/display-after-cleanup.txt`: final display service dump.

After integration, the test lease was released and no ADB forwards remained.
Only the task's own emulator, serial **`emulator-5580`** (host process **63484**),
was stopped successfully with:

```sh
.tools/android-sdk/platform-tools/adb -s emulator-5580 emu kill
```

The local Go server was left available for USB-device testing. Downloaded tools,
the isolated AVD, and generated artifacts remain under ignored `.tools/` and
`build/`; no global SDK or existing user AVD was changed.
