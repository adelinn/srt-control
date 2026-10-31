# CLAUDE.md

Context for working on this repo with Claude Code.

## What this is

`srt_audio_bridge.py` is a single-file, cross-platform app: receive audio over SRT (or a local input device), monitor it on a chosen output device, and re-send it over SRT. tkinter GUI by default, `--headless` for unattended runs, `--list-devices` for diagnostics. Dependencies: Python stdlib + numpy + sounddevice (PortAudio) + an ffmpeg binary built with libsrt. Keep it single-file and dependency-light unless there's a strong reason.

## Architecture

- `SrtSource`: one ffmpeg process decodes SRT to s16le 48 kHz stereo on stdout; the thread reads 10 ms blocks. Restarts with backoff (listeners always retry after 1 s).
- `DeviceSource`: PortAudio input callback; mono inputs are duplicated to stereo.
- `Engine._on_pcm`: fan-out point. Applies the real-time governor, updates peak meters, pushes to monitor and sender.
- `MonitorSink`: PortAudio output callback with a jitter buffer (`monitor_buffer_ms`). Re-primes on underrun, trims to target when the buffer exceeds 2x target + 50 ms.
- `SrtSender`: separate ffmpeg process, PCM on stdin, MPEG-TS over SRT out. Bounded queue (200 ms) that drops the oldest block. Independent restart loop, so a dead destination never affects the source or monitor.
- `App`: tkinter GUI. Polls the engine every 50 ms for logs, meters and status. Engine code must stay GUI-agnostic (headless mode reuses it).

## Non-obvious decisions (don't undo without re-testing)

- **PortAudio for devices, not ffmpeg:** ffmpeg has no way to pick a playback device on Windows, and its device syntax differs per OS (dshow/avfoundation/pulse/alsa).
- **Sender input probe:** `-probesize 32 -analyzeduration 0` on the raw PCM input. With defaults, ffmpeg buffers seconds of PCM before starting, and that becomes permanent latency downstream.
- **Sender gating:** ffmpeg (6.1 verified) opens the SRT output only after reading some input. The sender primes 50 ms of silence, then holds real audio until the first `-progress` block, which means the output is open. Without this, a listener-mode output waiting for its receiver fills the pipe with stale audio. In caller mode it falls back after 3 s for old ffmpeg builds that only report once fed.
- **Real-time governor** (`MAX_AHEAD_S = 0.2`): drops audio arriving faster than real time (decoder probe burst, reconnects). Re-anchors when the source falls 0.5 s behind. Drops are logged at most every 10 s.
- **Keepalive silence** starts after 0.5 s without source data. Don't lower it much: SRT/TS delivers PCM in bursts of ~55 ms or more (more at low bitrates).
- **Units:** ffmpeg's SRT `latency` and `timeout` are microseconds; the GUI uses ms. `timeout` on the input does not affect how long a listener waits for a caller (verified).
- **Secrets:** `mask_secrets()` masks `passphrase=` in all logs, including ffmpeg's own error lines. The settings file is 0600, and passphrases are only saved when `save_passphrases` is set. Repeated identical ffmpeg errors are shown once per 60 s (`pump_stderr`).

## Testing

- `python tests/e2e_relay.py` and `python tests/e2e_relay.py --out-mode listener --join-after 5`. Python-paced tone pulses -> SRT -> bridge (headless, monitor off) -> SRT -> ffmpeg receiver. Checks every pulse arrives, pulse length is intact (no dropouts) and latency is under `--max-latency`. Measured on ffmpeg 6.1.1: about 400 ms end to end, the same whether the receiver joins immediately or late.
- Codecs verified through the relay: AAC, MP2, Opus (libopus), PCM SMPTE 302M (`-strict -2`).
- Gotcha: in some containers an SRT listener bound to 127.0.0.1 fails with "Address already in use". Bind listeners to 0.0.0.0 (the default when Host is blank).
- No sound card? File-backed ALSA devices in `~/.asoundrc` make PortAudio see a speaker and a mic. They are unclocked (the monitor shows constant underruns, the input floods and the governor drops), so use them for wiring checks only, not timing:

  ```
  pcm.speakers_test { type file  slave.pcm "null"  file "/tmp/monitor_out.raw"  format "raw"
                      hint { show on description "Test speakers" } }
  pcm.mic_test { type file  slave.pcm "null"  file "/dev/null"  infile "/tmp/tone.raw"  format "raw"
                 hint { show on description "Test mic" } }
  ```

- GUI on a headless Linux box: `xvfb-run -a python srt_audio_bridge.py`.
- Real playback on actual hardware (Windows WASAPI, macOS Core Audio) has not been tested yet.

## Ideas not done yet

- Web control page (FastAPI or stdlib http.server) so the laptop can run headless and be controlled from a phone or the control room. Needs authentication.
- SRT link stats (RTT, loss, retransmits): ffmpeg doesn't expose them; would need srt-live-transmit or libsrt bindings.
- Preset dropdown (`--config` files already work as presets), peak hold and clip indicator, confirm-on-Stop or "live lock", tray icon and autostart, PyInstaller builds per OS.
- Passthrough mode (no re-encode) for SRT-to-SRT, and drift-compensating resampling for very long monitor sessions.
