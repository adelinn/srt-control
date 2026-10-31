# CLAUDE.md

Context for working on this repo with Claude Code.

## What this is

SRT Audio Bridge, a cross-platform Go app with two inputs (SRT in, a local input device such as a mic) and two outputs (a monitor device, SRT out). Both inputs can run at once and each output picks one. The core requirement is talkback: hear the SRT programme feed on the monitor while the mic goes out over SRT, at the same time. Relaying SRT in to SRT out is also supported. Fyne GUI by default, `-headless` for unattended runs, `-list-devices` for diagnostics. Dependencies: Go stdlib + Fyne (GUI) + malgo/miniaudio (audio devices, compiled in) + an ffmpeg binary built with libsrt, run as a child process. Release builds embed that ffmpeg and unpack it when none is installed. Keep it dependency-light. It replaced an earlier single-file Python/tkinter version (in git history). Settings files from it still load: their single `source` becomes both `monitor_source` and `send_source`.

Primary target: Windows amd64, cross-compiled in GitHub Actions (`.github/workflows/build.yml`). macOS comes later and needs a macOS runner (cgo + Apple SDK).

## Layout

- `cmd/srt-audio-bridge/main.go`: flags, headless loop, `-list-devices`. `main.version` is set by CI via `-ldflags -X`. `console_windows.go` attaches to the parent terminal when there are command-line options.
- `internal/bridge`: the engine. No GUI imports; headless mode and the tests use it directly.
  - `config.go`: `Config`/`SrtEndpoint` (JSON keys match the Python version), URL building, validation, `MaskSecrets`.
  - `ffmpeg.go`: finding and probing ffmpeg, `ffproc` (start/stop), stderr pump with de-duplication.
  - `bundle.go` / `bundle_embed.go`: the embedded ffmpeg (only with `-tags bundleffmpeg`; CI fills `bundled/`, which is gitignored), unpacking it, `ResolveFFmpeg`.
  - `proc_windows.go` / `proc_other.go`: hidden console windows, SIGTERM vs Kill, Windows job object.
  - `audio.go`: miniaudio backends, device listing and opening (always 48 kHz s16 stereo).
  - `source.go`: `srtSource` (ffmpeg decode, restart loop) and `deviceSource` (capture callback).
  - `monitor.go`: `monitorSink`, jitter buffer and volume.
  - `sender.go`: `srtSender`, ffmpeg encode, gating, keepalive silence.
  - `engine.go`: `Engine`, wiring, the real-time governor, peaks, status snapshot.
- `internal/gui`: Fyne UI. `app.go` (window, widgets <-> `bridge.Config`, polling), `meter.go` (level meter), `widgets.go` (panel, status light, state badge), `theme.go`, `layouts.go`, `native_windows.go` (Win32 Open dialog and MessageBox) / `native_other.go` (falls back to Fyne dialogs).

## Architecture

- `srtSource`: one ffmpeg process decodes SRT to s16le 48 kHz stereo on stdout; the goroutine reads 10 ms blocks. Restarts with backoff (listeners always retry after 1 s).
- `deviceSource`: miniaudio capture callback; miniaudio turns mono inputs into stereo.
- `Engine`: `paths[inSRT]` and `paths[inDevice]`, each with its own real-time governor and peak meter (`Peaks`, `Status` report both). An input only runs while an output uses it; `SetMonitor` can switch the monitor's input live and starts that input if needed. `onPCM(in, data)` pushes a block to the monitor if `monitorFrom == in` and to the sender if `sendFrom == in` (no mixing). Monitor and sender are atomic pointers because the audio threads read them while the UI swaps them; `mu` serializes Start/Stop/SetMonitor.
- `openInputDevice` / `openMonitor` are package vars so tests can swap in a fake mic and monitor (`talkback_test.go`).
- `monitorSink`: miniaudio playback callback with a jitter buffer (`MonitorBufferMs`). Re-primes on underrun, trims to target when the buffer exceeds 2x target + 50 ms.
- `srtSender`: separate ffmpeg process, PCM on stdin, MPEG-TS over SRT out. Bounded channel (20 blocks = 200 ms) that drops the oldest block. Independent restart loop, so a dead destination never affects the source or monitor.
- GUI: polls the engine every 50 ms (`fyne.Do`) for log lines, meters and status. Engine code must stay GUI-agnostic.

## Non-obvious decisions (don't undo without re-testing)

- **miniaudio for devices, not ffmpeg:** ffmpeg has no way to pick a playback device on Windows, and its device syntax differs per OS. miniaudio is compiled in (no DLL), enumerates fresh on every call (rescan works while running), and converts rate/format/channels, so the engine only ever sees 48 kHz s16 stereo.
- **Device IDs are pinned** (`runtime.Pinner`) while `malgo.InitDevice` runs, because the C config points at Go memory. Devices are saved by name, not ID; Python-era labels `"Name  [API]"` still match (`matchDevice`).
- **Sender input probe:** `-probesize 32 -analyzeduration 0` on the raw PCM input. With defaults, ffmpeg buffers seconds of PCM before starting, and that becomes permanent latency downstream.
- **Sender gating:** ffmpeg (6.1 verified) opens the SRT output only after reading some input. The sender primes 50 ms of silence, then holds real audio until the first `-progress` block, which means the output is open. Without this, a listener-mode output waiting for its receiver fills the pipe with stale audio. In caller mode it falls back after 3 s for old ffmpeg builds that only report once fed.
- **Real-time governor** (`maxAhead = 0.2` s): drops audio arriving faster than real time (decoder probe burst, reconnects). Re-anchors when the source falls 0.5 s behind. Drops are logged at most every 10 s.
- **Keepalive silence** starts after 0.5 s without source data. Don't lower it much: SRT/TS delivers PCM in bursts of ~55 ms or more (more at low bitrates).
- **Units:** ffmpeg's SRT `latency` and `timeout` are microseconds; the GUI uses ms. `timeout` on the input does not affect how long a listener waits for a caller (verified).
- **Stopping ffmpeg:** close stdin first (an encoder then flushes and closes SRT cleanly, the only graceful option on Windows), then SIGTERM (Kill on Windows), then Kill after 1.5 s. `cmd.Wait` runs after the stderr pump hits EOF.
- **No orphaned ffmpeg:** on Windows every ffmpeg joins a kill-on-close job object, so Windows ends them when the app exits for any reason. Fyne itself quits its loop on SIGINT/SIGTERM; `gui.Run` stops the engine after `ShowAndRun` returns, and the signal handler only marks the UI closing so late `fyne.Do` polls don't touch widgets off the main thread.
- **ffmpeg lookup** (`FindFFmpeg`, `ResolveFFmpeg`): an explicit path; for the default "ffmpeg", next to the exe, then PATH (skipped if it has no SRT and a bundle exists), then the unpacked bundle. If nothing is found, or the configured path is gone, the bundle is unpacked to `os.UserCacheDir()/SRT Audio Bridge/ffmpeg/<sha256[:12]>/` (`%LOCALAPPDATA%` on Windows): temp file, SHA-256 check, rename, then the `installed.sha256` marker last. Other version folders are deleted afterwards.
- **Bundled ffmpeg is BtbN's LGPL release-branch build** (newest `ffmpeg-n*-latest-win64-lgpl-*.zip`, verified against `checksums.sha256`, and the binary must contain `--enable-libsrt`/`--enable-libopus`). LGPL rather than GPL for lighter redistribution terms; its license and a source note ship in the zip and next to the unpacked binary. Embedded gzip-compressed (stdlib only, fast to unpack).
- **One Windows exe:** built with `-H=windowsgui` (no console on double-click). With command-line options it calls `AttachConsole(ATTACH_PARENT_PROCESS)` and reopens `CONOUT$` unless output is redirected. Shells don't wait for it. A separate console exe was dropped so the embedded ffmpeg isn't shipped twice.
- **Native dialogs on their own OS thread** (`native_windows.go`), owned by the Fyne window: no re-entrancy into GLFW's loop, and the window keeps drawing while the dialog is open.
- **Secrets:** `MaskSecrets()` masks `passphrase=` in all logs, including ffmpeg's own error lines. The settings file is 0600, and passphrases are only saved when `SavePassphrases` is set. Repeated identical ffmpeg errors are shown once per 60 s (`errorDedupe`).
- **Fonts:** Fyne's bundled font has no ✓/⚠-style symbols; keep labels to plain text and common punctuation (— · …).

## Testing

- `go test ./...` runs unit tests (`config_test.go`, `engine_test.go`, `bundle_test.go`, `internal/gui/app_test.go` with Fyne's headless test driver) and two end-to-end tests. `talkback_test.go`: a 440 Hz feed arrives over SRT while a fake mic plays 1 kHz; it checks that the monitor gets 440 Hz and the SRT receiver 1 kHz at the same time, then switches the monitor to the mic live. `relay_test.go`: Go-paced tone pulses -> ffmpeg -> SRT -> engine (monitor off) -> SRT -> ffmpeg receiver, in caller mode and with a receiver joining 5 s late. Checks every pulse arrives, pulse length is intact (no dropouts) and latency is under `-relay.max-latency`. Measured on ffmpeg 6.1.1: AAC ~400 ms, MP2 ~385 ms, Opus ~370 ms, PCM ~340 ms. Use `-race` when touching the engine.
- Codec choice: `go test ./internal/bridge -run Relay -v -args -relay.codec=Opus`.
- CI's Linux job (vet + all tests) is switched off for now with `if: false` in `build.yml`; only the Windows build runs. Run `go vet ./...` and `go test -race ./...` locally before pushing.
- Linux build deps: `gcc libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev`; Windows cross-compile: `gcc-mingw-w64-x86-64` (first build ~6 min, cached after). CI checks the exes only import system DLLs.
- Gotcha: in some containers an SRT listener bound to 127.0.0.1 fails with "Address already in use". Bind listeners to 0.0.0.0 (the default when Host is blank).
- No sound card? Use PulseAudio null sinks: they play in real time like a sound card and can be recorded. `apt install pulseaudio pulseaudio-utils`, then `pulseaudio -D --exit-idle-time=-1`, then `pactl load-module module-null-sink sink_name=studio sink_properties=device.description=Studio-monitors`. Pick "Studio-monitors" as the monitor and record it with `parec -d studio.monitor --format=s16le --rate=48000 --channels=2 > heard.raw`. For a mic, make a second sink, play a tone into it (`ffmpeg ... -f pulse -device micfeed x`) and pick "Monitor of ..." as the local input. Don't use ALSA `type file` devices for playback: they have no clock, so the monitor writes gigabytes of audio a second to the file.
- GUI on a headless Linux box: `xvfb-run -a go run ./cmd/srt-audio-bridge`; `FYNE_THEME=light|dark` forces a theme. Screenshots: `ffmpeg -f x11grab -video_size WxH -i :99 -frames:v 1 shot.png`. With no window manager, `xdotool windowclose` destroys the window instead of sending WM_DELETE_WINDOW, so it skips the close handler.
- Bundle logic: `bundle_test.go` uses shell-script fake ffmpegs (not on Windows). To try a real bundled build on Linux, put `gzip -9c /usr/bin/ffmpeg` and its sha256 (plus any LICENSE.txt/SOURCE.txt) into `internal/bridge/bundled/`, build with `-tags bundleffmpeg`, and run with `PATH=/nonexistent XDG_CACHE_HOME=/tmp/x`.
- Checked here on PulseAudio devices: talkback in the GUI (monitor plays SRT in while the mic goes out over SRT with Opus). Relay and live monitor switching are covered by the tests with stand-in devices. Not yet tested on real hardware: Windows WASAPI playback/capture, the native dialogs, the job object, console attach, unpacking into `%LOCALAPPDATA%`, and HiDPI scaling.

## Ideas not done yet

- Web control page so the laptop can run headless and be controlled from a phone or the control room. Needs authentication.
- SRT link stats (RTT, loss, retransmits): ffmpeg doesn't expose them; would need srt-live-transmit or libsrt bindings.
- macOS build job (see the commented block in the workflow), .app bundle, signing; Windows code signing.
- Bundle an ffmpeg in the macOS build too (the workflow comment shows where).
- Preset dropdown (`-config` files already work as presets), peak hold and clip indicator, confirm-on-Stop or "live lock", tray icon and autostart.
- Passthrough mode (no re-encode) for SRT-to-SRT, and drift-compensating resampling for very long monitor sessions.
