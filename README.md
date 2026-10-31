# SRT Audio Bridge

Listen to an SRT audio stream on any output device while sending a local input (a mic or line-in) to an SRT destination, both at the same time. It can also relay the SRT stream onward instead. One self-contained program with a desktop GUI and a headless mode, written in Go. GitHub Actions builds it for Windows (amd64); macOS is planned.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshot-dark.png">
  <img alt="SRT Audio Bridge window" src="docs/screenshot.png">
</picture>

```
inputs (both can run at once)                       outputs (each picks one input)

SRT in --ffmpeg decode--> PCM 48 kHz s16 stereo --+    Monitor  --> output device
                                                  +--> (plays SRT in or local input)
local input device (mic, line-in) ----------------+    SRT out  --> ffmpeg encode --> SRT
                                                       (sends local input or SRT in)
```

| Setup | Monitor plays | SRT output sends |
|---|---|---|
| Hear the programme feed, send your mic (default) | SRT input | Local input |
| Relay: hear the stream and forward it | SRT input | SRT input |
| Send your mic and hear yourself | Local input | Local input |

ffmpeg does the SRT, decoding and encoding. Audio devices go through [miniaudio](https://miniaud.io) (compiled into the program): WASAPI, DirectSound or MME on Windows, Core Audio on macOS, PulseAudio or ALSA on Linux. It converts sample rate and channel count, so any device works.

## Get it (Windows)

1. Download `srt-audio-bridge-windows-amd64` from the latest run under the repo's **Actions** tab, or a zip from **Releases** (published for `v*` tags).
2. Run `srt-audio-bridge.exe`. It isn't signed yet, so SmartScreen asks first: **More info → Run anyway**.

That's all: ffmpeg is built into the exe. ffmpeg is picked in this order:

1. The file set under **Settings**, if it's a path.
2. `ffmpeg.exe` next to the app.
3. An ffmpeg on `PATH`, if it supports SRT.
4. The built-in copy. The first time it's needed, the app unpacks it to `%LOCALAPPDATA%\SRT Audio Bridge\ffmpeg\`; no admin rights or internet needed. A new release replaces the old copy.

**Check** shows which one is used (`SRT OK · bundled` for the built-in one).

The built-in ffmpeg is an LGPL build from [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds) with SRT, AAC, MP2 and Opus. Its license and a note on its source come with the download (`ffmpeg-LICENSE.txt`, `ffmpeg-SOURCE.txt`) and are unpacked next to it.

## Run

```
srt-audio-bridge                        # GUI
srt-audio-bridge -list-devices          # print audio devices
srt-audio-bridge -headless              # run the last saved settings, no GUI
srt-audio-bridge -config venue-a.json   # use another settings file (presets)
srt-audio-bridge -version
```

On Windows the exe prints to the terminal it was started from, but cmd and PowerShell don't wait for it (it is a GUI program). When that matters, use `start /wait srt-audio-bridge.exe -list-devices` or `Start-Process -Wait -NoNewWindow srt-audio-bridge.exe -ArgumentList -headless`. Task Scheduler and service wrappers can run `-headless` directly.

Settings are saved to `~/.srt_audio_bridge.json` when you press Start and when you close the window. It is the same file and format the earlier Python version used, so existing settings carry over. Passphrases are only saved when "Save passphrases" is ticked.

## Behaviour

- **Inputs:** SRT (caller, listener or rendezvous, with latency, passphrase, stream ID) and a local input device, each with its own level meter. An input only runs while an output uses it. With an audio+video SRT feed, the first audio track is used.
- **Monitor:** what it plays (SRT input or local input), output device, volume, mute and buffer size can all be changed while running. **Devices** rescans without stopping. Inputs aren't mixed: the monitor plays one at a time.
- **SRT out:** MPEG-TS with AAC, MP2, Opus or PCM (SMPTE 302M), tuned for low latency: each audio frame is sent as soon as it is encoded (one PES packet per frame, no mux preload), and Opus uses its low-delay mode with 10 ms frames. Keeps the link up with silence while its input is down. What it sends and its settings apply when you press Start or Restart.
- **SRT URLs:** built from the fields, always with `transtype=live`, and shown under them. Tick **Edit URL** to type the URL yourself, for SRT options the fields don't cover (for example `rcvbuf` or `tlpktdrop`). It is then passed to ffmpeg exactly as typed. The edited URL shows the passphrase in plain text. Unless "Save passphrases" is ticked, the passphrase is removed from the URL when settings are saved.
- **Reconnects:** SRT in and SRT out reconnect on their own and independently, so a dead destination never stops monitoring, and a dropped programme feed never stops your mic going out.
- **Latency:** set SRT latency per endpoint; anything from 0 ms up is accepted, and the presets start at 20 ms. SRT uses the larger of the two ends' latencies, so the far end has to be set low too. Low values leave less time to resend lost packets, so only go low on a clean network. **Low-latency start** (default) and **Raw low latency** (`probesize=32`) make the SRT input decode almost at once after connecting; raw is about 40 ms faster to the first audio and slightly lower in steady state. Bursts of old audio (after a reconnect or ffmpeg's start-up probe) are dropped instead of becoming permanent delay. The monitor's buffer can go down to 20 ms.
- **Measured** end to end through two SRT hops, including the test's own sending encoder (AAC) and receiver:

  | SRT latency per hop | Output codec | End to end |
  |---|---|---|
  | 120 ms | AAC | ~380 ms |
  | 20 ms, raw low latency | AAC | ~165 ms |
  | 20 ms, raw low latency | MP2 | ~145 ms |
  | 20 ms, raw low latency | Opus | ~135 ms |
  | 20 ms, raw low latency | PCM (SMPTE 302M) | ~105 ms |
- **Shutdown:** closing the window, Ctrl+C or SIGTERM stops ffmpeg cleanly. On Windows, ffmpeg is also ended if the app crashes or is killed, so no stray ffmpeg keeps holding an SRT port.

## Platform notes

- **Windows:** WASAPI is picked by default, and miniaudio resamples devices that aren't set to 48 kHz. Picking ffmpeg and error messages use the native Windows dialogs. The GUI needs OpenGL 2.1, which any GPU driver provides; a VM or remote session without GPU acceleration may not. If company policy blocks programs in `%LOCALAPPDATA%`, the built-in ffmpeg can't run from there; put `ffmpeg.exe` next to the app instead.
- **macOS (planned):** the first time you use a local input, allow microphone access.
- **Security:** passphrases are masked in the log, and the settings file is created with 0600 permissions. ffmpeg still receives them on its command line, so other local users can see them in the process list.

## Build from source

You need Go 1.24+ and a C compiler, because miniaudio and the GUI toolkit ([Fyne](https://fyne.io)) use cgo.

- **Linux:**
  ```
  sudo apt install gcc libgl1-mesa-dev xorg-dev libwayland-dev libxkbcommon-dev
  go build ./cmd/srt-audio-bridge
  ```
- **Windows exe from Linux** (what CI does; the first build takes a few minutes):
  ```
  sudo apt install gcc-mingw-w64-x86-64
  CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
    go build -ldflags "-s -w -H=windowsgui" -o srt-audio-bridge.exe ./cmd/srt-audio-bridge
  ```
  CI also embeds the icon, manifest and version info with [go-winres](https://github.com/tc-hib/go-winres), and the ffmpeg build: it puts `ffmpeg.gz`, `ffmpeg.sha256`, `LICENSE.txt` and `SOURCE.txt` into `internal/bridge/bundled/` and builds with `-tags bundleffmpeg`. Without that tag the program has no built-in ffmpeg and uses an installed one. See `.github/workflows/build.yml`.
- **Windows natively:** put a MinGW-w64 `gcc` (MSYS2 or WinLibs) on `PATH`, then `go build -ldflags -H=windowsgui ./cmd/srt-audio-bridge`.
- **macOS (planned):** Xcode command line tools, then `go build ./cmd/srt-audio-bridge`.

## Test

```
go test ./...                                                  # unit tests + end-to-end tests
go test ./internal/bridge -run Talkback -v                     # hear SRT in while sending the mic
go test ./internal/bridge -run Relay -v -args -relay.codec=Opus   # relay with another output codec
go test ./internal/bridge -run Relay/calls -v -args -relay.latency=20 -relay.raw -relay.fast-harness   # lowest latency
go test -short ./...                                           # skip the end-to-end tests
```

The end-to-end tests run real ffmpeg and SRT but need no sound card:

- **Talkback:** a programme feed arrives over SRT while a mic tone goes out, at the same time. The test checks that the monitor plays the feed, the far end receives the mic, and switching the monitor to the mic works while running. The mic and monitor are stand-ins.
- **Relay:** tone pulses go over SRT through the engine to a receiver: with the bridge calling the destination, with the destination joining 5 s late, and through hand-edited URLs. It checks every pulse arrives intact within the latency limit and logs the median latency.
