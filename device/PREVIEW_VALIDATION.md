# Native preview profile validation

Date: 2026-11-16. Passed on the isolated official Google APIs Android 14/API 34
arm64 emulator. This test used the actual shell runner, MediaCodec output and
direct wire connections. It did not exercise Go/OIDC/browser integration.

## Environment and build

- Serial `emulator-5580`, host emulator PID `83998` at handoff.
- Official emulator 37.2.13, Google APIs API 34 arm64 image revision 14.
- Fingerprint:
  `google/sdk_gphone64_arm64/emu64a:14/UE1A.230829.050/12077443:userdebug/dev-keys`.
- Two guest CPUs (`0-1`), 1536 MiB configured with `-qemu -m 1536`;
  guest `MemTotal: 1511076 kB` after reservations.
- Logical display 720×1280, rotation 0. Captured content was the stock launcher.
- `bash scripts/build-device.sh` produced the tested Java 8/API 34 dex JAR.
- Tested JAR SHA-256:
  `f47352318079c9e46a9fa800e9e4a08ce2f816aa59ce0d5ffde8baacd08a6396`.

The existing `build/emulator/start.py` launched only this task's AVD, with
`-port 5580 -cores 2 -memory 1536 -no-window -no-audio -no-snapshot -gpu swiftshader
-no-boot-anim -no-metrics -qemu -m 1536`. No servers were started on 8081/8082,
and no other devices were targeted.

## Procedure and observed frames

`build/emulator/profile-probe.py` pushed a separate direct-test JAR, launched
`app_process` with `--socket marionette_direct_test --max-size 480 --max-fps 8
--bit-rate 600000`, and created its own ephemeral adb forward on port 57015.
The same video and control TCP connections remained open across both profile
changes. The control connection also remained open through a deliberate video
disconnect/reconnect.

| Stage | Encoded dimensions | Captured access units | Keyframes | AVC frame bytes |
| --- | --- | ---: | ---: | ---: |
| Initial preview | 270×480 | 3 | 1 | 8145 |
| Switch to interactive | 720×1280 | 3 | 2 | 47306 |
| Switch back to preview | 270×480 | 3 | 3 | 24056 |
| Video reconnect in preview | 270×480 | 3 | 3 | 24057 |

Every stage delivered metadata, actual 32-byte Annex B SPS/PPS (NAL types 7 and
8), then a flagged keyframe containing an actual type-5 IDR NAL. Codec metadata
was `avc1.42C029`. Metadata/configuration were repeated twice in each initial or
reconfigured capture; reconnect delivered one pair. No socket replacement was
needed for the two profile changes.

The same control socket successfully answered `info`, `keyframe` and both valid
`videoProfile` requests. All response IDs matched. There were 21 control
responses, including explicit `ok:false` for profile `"bogus"` and numeric profile
`123`. Subsequent `info` and reconnect still succeeded. Observed response durations
were 0–5 ms in this short local run; this is not a performance benchmark.

## Real decoding and rate limits

The FFmpeg skill's `probe.py`, `export.py --preset copy --repair`, and `look.py`
processed all four original AVC captures. The lossless MP4 remuxes each retained
all three frames. Each reported **zero decode errors before and after remux**.
All four decoded PNGs were inspected and visibly contained the launcher icons,
wallpaper and status/navigation areas. Interactive output had visibly finer
detail than preview. PNG display scaling and timecode overlays come from the
inspection tool, not the capture driver.

Preview requests a maximum edge of 480, maximum frame rate of 8 and target bit
rate of 600000 bit/s. Interactive requests 1280, 30 and 4000000 respectively.
These are configured targets, not guaranteed delivered FPS or measured bit rates.
The three-frame samples had PTS deltas of 125000 µs in preview and 33333 µs in
interactive. Such tiny static-screen samples do not establish sustained FPS.
Keyframes were explicitly requested during sampling, so byte counts also do not
represent ordinary steady-state bandwidth.

Raw Annex B has no container timestamps. FFmpeg inferred 25 fps when remuxing;
that is a tooling default, **not the driver frame rate**. Collection wall times
(13, 1162, 805 and 365 ms) include buffering, command processing and recreation;
they are not end-to-end input/video latency measurements.

No device-source fault was observed, so no device implementation was changed
for this QA. No physical-device, Android 11–13, OEM or long-running-load claim is
made. The later Go/OIDC/browser run also passed preview → interactive → preview;
its separate observations are in
[the dashboard validation record](../docs/AUTH_DASHBOARD_VALIDATION.md).

## Artifacts and cleanup

Artifacts are under `build/emulator/`:

- `profile-probe.py`, `profile-results.json`, `profile-driver.log`.
- `profile-preview-initial`, `profile-interactive`, `profile-preview-return`,
  `profile-reconnect`: each has `.h264`, `.mp4`, `.png`, `-probe.json`,
  `-remux-plan.json`, `-remux.json`, `-decode-plan.json`, `-decode.json`.
- `profile-display-after-cleanup.txt`: only the built-in display remains.

Direct driver PID 3562 was stopped with the existing ownership-checking
`stop-driver.py`. Only forward 57015 was removed. Its remote JAR and PID file
were removed. An immediate display dump still showed asynchronous teardown;
the subsequent dump confirmed that the virtual display was gone. No direct-test
forward remained at handoff.

At the direct-test handoff, emulator **emulator-5580**, host PID **83998**, was left
running for browser integration QA. That QA subsequently completed. The parent
then stopped this emulator using:

```sh
.tools/android-sdk/platform-tools/adb -s emulator-5580 emu kill
```
