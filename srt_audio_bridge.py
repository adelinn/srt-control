#!/usr/bin/env python3
"""
SRT Audio Bridge
================
Receive audio over SRT (or from a local input device), monitor it on any output
device, and forward it to another SRT destination - with a small GUI.

Who does what
  ffmpeg       SRT in/out, decoding and encoding (needs an ffmpeg built with libsrt)
  sounddevice  audio device I/O through PortAudio: WASAPI/MME/DirectSound on
               Windows, Core Audio on macOS, ALSA/PulseAudio/JACK on Linux.
               ffmpeg alone can't pick a playback device on Windows and its device
               syntax differs on every OS, so PortAudio gives one model everywhere.

Signal flow
  SRT in ----ffmpeg decode----> PCM 48 kHz s16 stereo --+--> monitor output device
  local input device ---------------------------------->+--> ffmpeg encode --> SRT out

Install
  Python 3.9+ with tkinter     (Debian/Ubuntu: sudo apt install python3-tk
                                Homebrew Python: brew install python-tk)
  pip install sounddevice numpy
                               (Linux also: sudo apt install libportaudio2)
  ffmpeg with SRT support on PATH, or pick the binary in the GUI.
      check: ffmpeg -hide_banner -protocols | grep srt

Run
  python srt_audio_bridge.py                   GUI
  python srt_audio_bridge.py --list-devices    print audio devices and exit
  python srt_audio_bridge.py --headless        run the saved settings without a GUI
  python srt_audio_bridge.py --config FILE     use another settings file (presets)

Notes
  * ffmpeg's SRT "latency" and "timeout" options are in microseconds; the GUI
    takes milliseconds and converts.
  * Passphrases are masked in the log and are only written to the settings file
    when "Save passphrases" is ticked. They are still visible to local users in
    the process list (ffmpeg takes them on the command line).
"""
from __future__ import annotations

import argparse
import json
import math
import os
import queue
import re
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from urllib.parse import quote

try:
    import numpy as np
except ImportError:  # pragma: no cover
    sys.exit("numpy is required:  pip install numpy sounddevice")

try:
    import sounddevice as sd
    SD_ERROR = None
except Exception as exc:  # PortAudio missing etc. SRT-to-SRT relay still works.
    sd = None
    SD_ERROR = f"{type(exc).__name__}: {exc}"

try:
    import tkinter as tk
    from tkinter import filedialog, messagebox, ttk
except ImportError:
    tk = None

APP_NAME = "SRT Audio Bridge"
RATE = 48_000
CHANNELS = 2
FRAME_BYTES = CHANNELS * 2                # s16le stereo
BLOCK_FRAMES = 480                        # 10 ms
BLOCK_BYTES = BLOCK_FRAMES * FRAME_BYTES
MAX_AHEAD_S = 0.20      # audio arriving faster than real time beyond this is dropped
KEEPALIVE_AFTER_S = 0.5 # source silent this long -> send silence to keep SRT out alive
DEFAULT_CONFIG = Path.home() / ".srt_audio_bridge.json"
NO_WINDOW = getattr(subprocess, "CREATE_NO_WINDOW", 0)  # no console flashes on Windows

CODECS = {
    "AAC": ["-c:a", "aac"],
    "MP2": ["-c:a", "mp2"],
    "Opus": ["-c:a", "libopus"],
    "PCM (SMPTE 302M)": ["-c:a", "s302m", "-strict", "-2"],
}
LOSSLESS = {"PCM (SMPTE 302M)"}
BITRATES = (96, 128, 160, 192, 256, 320, 384)
SRT_MODES = ("caller", "listener", "rendezvous")
MODE_HINTS = {
    "caller": "Connects out to host:port.",
    "listener": "Waits for the other side to connect. Host blank = all interfaces.",
    "rendezvous": "Both sides connect to each other at the same time.",
}
# ffmpeg warnings that are expected here and only add noise to the log
LOG_NOISE = re.compile(r"Guessed Channel Layout|frame size not set|Estimating duration")
SECRET = re.compile(r"(passphrase=)[^&\s'\"]+")


def mask_secrets(text: str) -> str:
    return SECRET.sub(r"\1****", text)


# --------------------------------------------------------------------------
# Settings
# --------------------------------------------------------------------------
@dataclass
class SrtEndpoint:
    mode: str = "caller"
    host: str = ""
    port: int = 9000
    latency_ms: int = 120
    passphrase: str = ""
    streamid: str = ""

    def validate(self) -> None:
        if self.mode not in SRT_MODES:
            raise ValueError(f"unknown mode '{self.mode}'")
        if self.mode != "listener" and not self.host.strip():
            raise ValueError(f"host is required in {self.mode} mode")
        if not 1 <= int(self.port) <= 65535:
            raise ValueError("port must be 1-65535")
        if int(self.latency_ms) < 0:
            raise ValueError("latency can't be negative")
        if self.passphrase and not 10 <= len(self.passphrase) <= 79:
            raise ValueError("SRT passphrases must be 10-79 characters")

    def url(self, extra: dict | None = None, mask: bool = False) -> str:
        host = self.host.strip() or "0.0.0.0"
        if ":" in host and not host.startswith("["):  # bare IPv6 address
            host = f"[{host}]"
        params = {
            "mode": self.mode,
            "latency": int(self.latency_ms) * 1000,  # ffmpeg wants microseconds
            "pkt_size": 1316,                        # 7 x 188-byte TS packets
        }
        params.update(extra or {})
        if self.streamid:
            params["streamid"] = self.streamid
        if self.passphrase:
            params["passphrase"] = "****" if mask else self.passphrase
        query = "&".join(f"{k}={quote(str(v), safe='')}" for k, v in params.items())
        return f"srt://{host}:{int(self.port)}?{query}"


@dataclass
class AppConfig:
    ffmpeg: str = "ffmpeg"
    source: str = "srt"                   # "srt" | "device"
    srt_in: SrtEndpoint = field(default_factory=lambda: SrtEndpoint(mode="listener", port=9000))
    low_latency_input: bool = True
    host_api: str = ""                    # "" = pick a sensible default per OS
    input_device: str = ""                # "" = system default
    monitor_enabled: bool = True
    monitor_device: str = ""              # "" = system default
    monitor_volume: float = 1.0
    monitor_muted: bool = False
    monitor_buffer_ms: int = 100
    send_enabled: bool = True
    srt_out: SrtEndpoint = field(default_factory=lambda: SrtEndpoint(mode="caller", host="127.0.0.1", port=9001))
    codec: str = "AAC"
    bitrate_k: int = 192
    keepalive_silence: bool = True
    save_passphrases: bool = False

    def to_dict(self) -> dict:
        d = asdict(self)
        if not self.save_passphrases:
            d["srt_in"]["passphrase"] = d["srt_out"]["passphrase"] = ""
        return d

    @classmethod
    def from_dict(cls, d: dict) -> "AppConfig":
        cfg = cls()
        for key, value in d.items():
            if key in ("srt_in", "srt_out") and isinstance(value, dict):
                ep = getattr(cfg, key)
                for k, v in value.items():
                    if hasattr(ep, k):
                        setattr(ep, k, v)
            elif hasattr(cfg, key):
                setattr(cfg, key, value)
        return cfg


def load_config(path: Path) -> AppConfig:
    try:
        return AppConfig.from_dict(json.loads(Path(path).read_text(encoding="utf-8")))
    except FileNotFoundError:
        return AppConfig()
    except (OSError, ValueError) as exc:
        print(f"Could not read {path}: {exc}; using defaults", file=sys.stderr)
        return AppConfig()


def save_config(cfg: AppConfig, path: Path) -> None:
    data = json.dumps(cfg.to_dict(), indent=2)
    # 0600: the file can hold passphrases
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        fh.write(data)


# --------------------------------------------------------------------------
# ffmpeg helpers
# --------------------------------------------------------------------------
def find_ffmpeg(hint: str) -> str | None:
    if hint and Path(hint).is_file():
        return hint
    return shutil.which(hint or "ffmpeg")


def ffmpeg_info(path: str) -> dict:
    """Version, SRT support and available encoders of an ffmpeg binary."""
    def run(*args):
        return subprocess.run([path, "-hide_banner", *args], capture_output=True, text=True,
                              timeout=15, creationflags=NO_WINDOW).stdout
    try:
        version = run("-version").splitlines()[0]
        protocols = run("-protocols")
        encoders = run("-encoders")
    except (OSError, subprocess.SubprocessError, IndexError) as exc:
        return {"ok": False, "error": str(exc)}
    m = re.search(r"version (\S+)", version)
    return {
        "ok": True,
        "version": m.group(1) if m else "unknown",
        "srt": bool(re.search(r"^\s*srt\s*$", protocols, re.M)),
        "encoders": set(re.findall(r"^\s*A[.\w]{5}\s+(\S+)", encoders, re.M)),
    }


def popen(cmd: list[str], **kw) -> subprocess.Popen:
    return subprocess.Popen(cmd, creationflags=NO_WINDOW, **kw)


def terminate(proc: subprocess.Popen | None) -> None:
    if proc is None:
        return
    if proc.poll() is None:
        try:
            proc.terminate()          # lets ffmpeg close the SRT socket cleanly
            proc.wait(timeout=1.5)
        except subprocess.TimeoutExpired:
            proc.kill()
        except OSError:
            pass
    for stream in (proc.stdin, proc.stdout, proc.stderr):
        try:
            if stream:
                stream.close()
        except OSError:
            pass


def pump_stderr(proc: subprocess.Popen, tag: str, log, seen: dict) -> None:
    """Forward ffmpeg's stderr to the log. While a far end is down, ffmpeg repeats
    the same errors on every retry; those repeats are shown once a minute."""
    def run():
        for raw in proc.stderr:
            line = raw.decode(errors="replace").strip()
            if not line or LOG_NOISE.search(line):
                continue
            key = re.sub(r" @ 0x[0-9a-f]+", "", line)
            now = time.monotonic()
            if now - seen.get(key, -1e9) < 60:
                continue
            seen[key] = now
            log(f"{tag}: {line}")
    threading.Thread(target=run, name=f"{tag}-stderr", daemon=True).start()


def cmdline(cmd: list[str]) -> str:
    return subprocess.list2cmdline(cmd) if os.name == "nt" else shlex.join(cmd)


# --------------------------------------------------------------------------
# Audio devices (PortAudio via sounddevice)
# --------------------------------------------------------------------------
@dataclass
class Device:
    index: int
    name: str
    api: str
    max_in: int
    max_out: int

    @property
    def label(self) -> str:
        return f"{self.name}  [{self.api}]"


def list_devices() -> list[Device]:
    if sd is None:
        return []
    apis = sd.query_hostapis()
    return [Device(i, d["name"], apis[d["hostapi"]]["name"], d["max_input_channels"],
                   d["max_output_channels"]) for i, d in enumerate(sd.query_devices())]


def find_device(label: str, kind: str) -> Device:
    """Resolve a saved label to a device. Indexes change when devices are
    plugged in or out, so match on the label, then on the name alone."""
    if sd is None:
        raise RuntimeError(f"audio devices unavailable ({SD_ERROR})")
    usable = [d for d in list_devices() if (d.max_in if kind == "input" else d.max_out) > 0]
    if label:
        for d in usable:
            if d.label == label:
                return d
        name = label.rsplit("  [", 1)[0]
        for d in usable:
            if d.name == name:
                return d
        raise RuntimeError(f"{kind} device not found: {label}")
    idx = sd.default.device[0 if kind == "input" else 1]
    for d in usable:
        if d.index == idx:
            return d
    if usable:
        return usable[0]
    raise RuntimeError(f"no {kind} devices found")


def default_host_api(devices: list[Device]) -> str:
    apis = {d.api for d in devices}
    for preferred in ("Windows WASAPI", "Core Audio"):
        if preferred in apis:
            return preferred
    return "All"


def portaudio_extras(dev: Device):
    if "WASAPI" in dev.api:
        try:  # let Windows resample if the device isn't set to 48 kHz
            return sd.WasapiSettings(auto_convert=True)
        except TypeError:  # older sounddevice
            return None
    return None


# --------------------------------------------------------------------------
# Engine components
# --------------------------------------------------------------------------
class Component:
    def __init__(self, log):
        self.log = log
        self.state = "idle"
        self.detail = ""
        self.stop_evt = threading.Event()
        self.seen_errors: dict[str, float] = {}

    def set(self, state: str, detail: str = "") -> None:
        self.state, self.detail = state, detail

    def status(self) -> tuple[str, str]:
        return self.state, self.detail


class SrtSource(Component):
    """ffmpeg reads SRT, decodes the first audio track to PCM on stdout.
    Restarts on its own when the stream drops or the far end isn't up yet."""

    def __init__(self, cfg: AppConfig, ffmpeg: str, on_pcm, log):
        super().__init__(log)
        self.cfg, self.ffmpeg, self.on_pcm = cfg, ffmpeg, on_pcm
        self.proc = None
        self.last_data = 0.0
        self.thread = threading.Thread(target=self._run, name="srt-in", daemon=True)

    def command(self) -> list[str]:
        cmd = [self.ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "warning"]
        if self.cfg.low_latency_input:
            cmd += ["-fflags", "nobuffer", "-flags", "low_delay",
                    "-probesize", "32768", "-analyzeduration", "250000"]
        url = self.cfg.srt_in.url(extra={"timeout": 5_000_000})  # µs: give up on a stalled stream
        cmd += ["-i", url, "-map", "0:a:0", "-vn", "-sn", "-dn",
                "-ac", str(CHANNELS), "-ar", str(RATE), "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"]
        return cmd

    def start(self):
        self.thread.start()

    def stop(self):
        self.stop_evt.set()
        terminate(self.proc)
        self.thread.join(timeout=3)

    def status(self):
        if self.state == "receiving" and time.monotonic() - self.last_data > 1.5:
            return "no audio", self.detail
        return self.state, self.detail

    def _run(self):
        ep = self.cfg.srt_in
        where = f"{ep.host.strip() or '0.0.0.0'}:{ep.port}"
        backoff = 1.0
        first = True
        while not self.stop_evt.is_set():
            self.set("connecting", ("listening on " if ep.mode == "listener" else "connecting to ") + where)
            cmd = self.command()
            if first:
                self.log("SRT in: " + cmdline(cmd))
                first = False
            try:
                self.proc = proc = popen(cmd, stdin=subprocess.DEVNULL,
                                         stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            except OSError as exc:
                self.set("error", f"can't start ffmpeg: {exc}")
                self.log(f"SRT in: can't start ffmpeg: {exc}")
                return
            pump_stderr(proc, "SRT in", self.log, self.seen_errors)
            receiving = False
            while True:
                data = proc.stdout.read(BLOCK_BYTES)
                if not data:
                    break
                data = data[: len(data) - len(data) % FRAME_BYTES]
                if not receiving:
                    receiving, backoff = True, 1.0
                    self.set("receiving", where)
                    self.log("SRT in: receiving audio")
                self.last_data = time.monotonic()
                self.on_pcm(data)
            terminate(proc)
            if self.stop_evt.is_set():
                break
            if receiving:
                self.log("SRT in: stream ended, reconnecting")
            self.set("reconnecting", f"retry in {backoff:.0f} s")
            self.stop_evt.wait(backoff)
            # a listener should be back on its port quickly; a caller backs off
            backoff = 1.0 if ep.mode == "listener" else min(backoff * 2, 8.0)
        self.set("stopped")


class DeviceSource(Component):
    """Local input device (mic/line-in/virtual cable) as the source."""

    def __init__(self, label: str, on_pcm, log):
        super().__init__(log)
        self.dev = find_device(label, "input")
        self.channels = min(CHANNELS, self.dev.max_in)
        self.on_pcm = on_pcm
        self.overflows = 0
        self.stream = None

    def start(self):
        self.stream = sd.InputStream(
            samplerate=RATE, channels=self.channels, dtype="int16", blocksize=BLOCK_FRAMES,
            device=self.dev.index, latency="low", callback=self._callback,
            extra_settings=portaudio_extras(self.dev))
        self.stream.start()
        self.set("capturing", self.dev.label)
        self.log(f"Input device: {self.dev.label}")

    def _callback(self, indata, frames, time_info, status):
        if status.input_overflow:
            self.overflows += 1
        pcm = indata if self.channels == CHANNELS else np.repeat(indata, CHANNELS, axis=1)
        self.on_pcm(pcm.tobytes())

    def stop(self):
        if self.stream:
            self.stream.stop()
            self.stream.close()
        self.set("stopped")

    def status(self):
        extra = f" · {self.overflows} overflows" if self.overflows else ""
        return self.state, self.detail + extra


class MonitorSink(Component):
    """Plays the PCM on an output device through a small jitter buffer.
    Excess is dropped so monitoring latency stays near the buffer size."""

    def __init__(self, label: str, buffer_ms: int, volume: float, muted: bool, log):
        super().__init__(log)
        self.dev = find_device(label, "output")
        self.channels = min(CHANNELS, self.dev.max_out)
        self.target = max(1, int(RATE * buffer_ms / 1000)) * FRAME_BYTES
        self.limit = self.target * 2 + RATE // 20 * FRAME_BYTES     # target x2 + 50 ms
        self.buf = bytearray()
        self.lock = threading.Lock()
        self.primed = False
        self.volume, self.muted = volume, muted
        self.underruns = self.drops = 0
        self.buffer_ms = buffer_ms
        self.stream = None

    def start(self):
        self.stream = sd.OutputStream(
            samplerate=RATE, channels=self.channels, dtype="int16", device=self.dev.index,
            latency="low", callback=self._callback, extra_settings=portaudio_extras(self.dev))
        self.stream.start()
        self.set("playing", self.dev.label)
        self.log(f"Monitor: {self.dev.label}")

    def push(self, data: bytes):
        with self.lock:
            self.buf += data
            if len(self.buf) > self.limit:
                del self.buf[: len(self.buf) - self.target]
                self.drops += 1

    def _callback(self, outdata, frames, time_info, status):
        need = frames * FRAME_BYTES
        chunk = None
        with self.lock:
            if not self.primed and len(self.buf) >= self.target:
                self.primed = True
            if self.primed and len(self.buf) >= need:
                chunk = self.buf[:need]
                del self.buf[:need]
            elif self.primed:
                self.primed = False       # underrun: refill to target before playing again
                self.underruns += 1
        if chunk is None or self.muted:
            outdata.fill(0)
            return
        pcm = np.frombuffer(chunk, np.int16).reshape(-1, CHANNELS)
        if self.volume != 1.0:
            pcm = np.clip(pcm * np.float32(self.volume), -32768, 32767).astype(np.int16)
        if self.channels == 1:
            outdata[:, 0] = pcm.mean(axis=1).astype(np.int16)
        else:
            outdata[:] = pcm

    def stop(self):
        if self.stream:
            self.stream.stop()
            self.stream.close()
        self.set("stopped")

    def status(self):
        if self.state != "playing":
            return self.state, self.detail
        state = "playing" if self.primed else "buffering"
        return state, f"{self.buffer_ms} ms buffer · {self.underruns} underruns · {self.drops} trims"


class SrtSender(Component):
    """ffmpeg encodes PCM from stdin and sends MPEG-TS over SRT.
    Runs independently of the source so a dead destination never stops monitoring."""

    def __init__(self, cfg: AppConfig, ffmpeg: str, log):
        super().__init__(log)
        self.cfg, self.ffmpeg = cfg, ffmpeg
        self.q: queue.Queue[bytes] = queue.Queue(maxsize=20)   # 200 ms of 10 ms blocks
        self.flowing = threading.Event()
        self.proc = None
        self.bitrate = ""
        self.thread = threading.Thread(target=self._run, name="srt-out", daemon=True)

    def command(self) -> list[str]:
        cmd = [self.ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "warning",
               "-nostats", "-progress", "pipe:1",
               # tiny probe: by default ffmpeg buffers seconds of raw PCM before it
               # starts, and that buffer becomes permanent latency downstream
               "-probesize", "32", "-analyzeduration", "0",
               "-f", "s16le", "-ar", str(RATE), "-ac", str(CHANNELS), "-i", "pipe:0"]
        cmd += CODECS[self.cfg.codec]
        if self.cfg.codec not in LOSSLESS:
            cmd += ["-b:a", f"{int(self.cfg.bitrate_k)}k"]
        cmd += ["-f", "mpegts", "-flush_packets", "1", "-muxdelay", "0", self.cfg.srt_out.url()]
        return cmd

    def start(self):
        self.thread.start()

    def stop(self):
        self.stop_evt.set()
        terminate(self.proc)
        self.thread.join(timeout=3)

    def push(self, data: bytes):
        try:
            self.q.put_nowait(data)
        except queue.Full:            # drop the oldest block rather than build latency
            try:
                self.q.get_nowait()
            except queue.Empty:
                pass
            try:
                self.q.put_nowait(data)
            except queue.Full:
                pass

    def _clear(self):
        with self.q.mutex:
            self.q.queue.clear()

    def _progress(self, proc):
        for raw in proc.stdout:
            key, _, value = raw.decode(errors="replace").strip().partition("=")
            if key == "bitrate":
                self.bitrate = "" if "N/A" in value else value.strip().replace("kbits/s", " kbit/s")
            elif key == "progress":
                if not self.flowing.is_set():
                    self.flowing.set()
                    self.log("SRT out: connected")
                self.set("sending", self.bitrate)

    def _wait_until_open(self, proc) -> bool:
        """ffmpeg reports progress once the SRT output is open. Until then real
        audio is held back, so nothing stale piles up in the pipe while a
        listener waits for its receiver (that backlog would become latency)."""
        proc.stdin.write(bytes(5 * BLOCK_BYTES))   # 50 ms silence lets ffmpeg open its input
        t0 = time.monotonic()
        while not self.flowing.wait(0.1):
            if self.stop_evt.is_set() or proc.poll() is not None:
                return False
            # very old ffmpeg builds only report once fed; a caller connects fast anyway
            if self.cfg.srt_out.mode != "listener" and time.monotonic() - t0 > 3:
                break
        return True

    def _run(self):
        ep = self.cfg.srt_out
        backoff = 1.0
        first = True
        while not self.stop_evt.is_set():
            self.flowing.clear()
            self._clear()
            self.bitrate = ""
            self.set("connecting", "waiting for a receiver" if ep.mode == "listener"
                     else f"connecting to {ep.host}:{ep.port}")
            cmd = self.command()
            if first:
                self.log("SRT out: " + cmdline(cmd))
                first = False
            try:
                self.proc = proc = popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                         stderr=subprocess.PIPE, bufsize=0)
            except OSError as exc:
                self.set("error", f"can't start ffmpeg: {exc}")
                self.log(f"SRT out: can't start ffmpeg: {exc}")
                return
            pump_stderr(proc, "SRT out", self.log, self.seen_errors)
            threading.Thread(target=self._progress, args=(proc,), daemon=True).start()
            try:
                if self._wait_until_open(proc):
                    self._clear()
                    last = time.monotonic()
                    idle = False
                    while not self.stop_evt.is_set() and proc.poll() is None:
                        try:
                            chunk = self.q.get(timeout=0.1)
                            idle = False
                        except queue.Empty:
                            gap = time.monotonic() - last
                            if not self.cfg.keepalive_silence or (not idle and gap < KEEPALIVE_AFTER_S):
                                continue
                            idle = True   # source is down: keep the stream alive with silence
                            chunk = bytes(int(min(gap, 1.0) * RATE) * FRAME_BYTES)
                        proc.stdin.write(chunk)
                        last = time.monotonic()
            except (BrokenPipeError, OSError, ValueError):
                pass
            was_flowing = self.flowing.is_set()
            terminate(proc)
            if self.stop_evt.is_set():
                break
            if was_flowing:
                backoff = 1.0
                self.log("SRT out: connection closed, restarting")
            self.set("reconnecting", f"retry in {backoff:.0f} s")
            self.stop_evt.wait(backoff)
            backoff = 1.0 if ep.mode == "listener" else min(backoff * 2, 8.0)
        self.set("stopped")


class Engine:
    """Wires a source to the monitor and the SRT sender."""

    def __init__(self, cfg: AppConfig, log):
        self.cfg = cfg
        self.log = lambda msg: log(mask_secrets(msg))
        self.source = self.monitor = self.sender = None
        self.running = False
        self._peak = np.zeros(CHANNELS, np.float32)
        self._peak_lock = threading.Lock()
        self._clock_ref: float | None = None
        self._clock_frames = 0
        self._dropped = 0
        self._dropped_logged_at = 0.0

    # -- lifecycle ---------------------------------------------------------
    def start(self):
        cfg = self.cfg
        ffmpeg = None
        if cfg.source == "srt" or cfg.send_enabled:
            ffmpeg = find_ffmpeg(cfg.ffmpeg)
            if not ffmpeg:
                raise RuntimeError("ffmpeg not found. Install it or set its path.")
        for label, ep, used in (("SRT in", cfg.srt_in, cfg.source == "srt"),
                                ("SRT out", cfg.srt_out, cfg.send_enabled)):
            if used:
                try:
                    ep.validate()
                except ValueError as exc:
                    raise RuntimeError(f"{label}: {exc}") from None
        if (cfg.source == "srt" and cfg.send_enabled and cfg.srt_in.mode == "listener"
                and cfg.srt_out.mode == "listener" and int(cfg.srt_in.port) == int(cfg.srt_out.port)):
            raise RuntimeError("SRT in and SRT out can't both listen on the same port")
        if cfg.codec not in CODECS:
            raise RuntimeError(f"unknown codec {cfg.codec}")
        try:
            if cfg.monitor_enabled:
                self._open_monitor()
            if cfg.send_enabled:
                self.sender = SrtSender(cfg, ffmpeg, self.log)
                self.sender.start()
            if cfg.source == "srt":
                self.source = SrtSource(cfg, ffmpeg, self._on_pcm, self.log)
            else:
                self.source = DeviceSource(cfg.input_device, self._on_pcm, self.log)
            self.source.start()
        except Exception:
            self.stop()
            raise
        self.running = True

    def stop(self):
        for comp in (self.source, self.sender, self.monitor):
            if comp:
                try:
                    comp.stop()
                except Exception as exc:  # keep stopping the rest
                    self.log(f"stop: {exc}")
        self.source = self.sender = self.monitor = None
        self.running = False

    # -- live controls -----------------------------------------------------
    def _open_monitor(self):
        c = self.cfg
        m = MonitorSink(c.monitor_device, c.monitor_buffer_ms, c.monitor_volume, c.monitor_muted, self.log)
        m.start()
        self.monitor = m

    def set_monitor(self, enabled: bool, label: str, buffer_ms: int):
        old, self.monitor = self.monitor, None
        if old:
            old.stop()
        self.cfg.monitor_enabled, self.cfg.monitor_device = enabled, label
        self.cfg.monitor_buffer_ms = buffer_ms
        if enabled and self.running:
            self._open_monitor()

    def set_volume(self, volume: float, muted: bool):
        self.cfg.monitor_volume, self.cfg.monitor_muted = volume, muted
        m = self.monitor
        if m:
            m.volume, m.muted = volume, muted

    # -- audio path --------------------------------------------------------
    def _on_pcm(self, data: bytes):
        frames = len(data) // FRAME_BYTES
        now = time.monotonic()
        # Real-time governor: after a probe or a reconnect, ffmpeg can hand over
        # a burst of old audio at once. Passing it on would add that much latency
        # to the monitor and to everything downstream, so drop what's too early.
        if self._clock_ref is None or self._clock_frames / RATE - (now - self._clock_ref) < -0.5:
            self._clock_ref, self._clock_frames = now, 0
        if (self._clock_frames + frames) / RATE - (now - self._clock_ref) > MAX_AHEAD_S:
            self._dropped += frames
            return
        if self._dropped and now - self._dropped_logged_at > 10:   # report at most every 10 s
            ms = self._dropped * 1000 // RATE
            if ms >= 20:
                self.log(f"Dropped {ms} ms of audio that arrived faster than real time (keeps latency low)")
            self._dropped, self._dropped_logged_at = 0, now
        self._clock_frames += frames
        pcm = np.frombuffer(data, np.int16).reshape(-1, CHANNELS)
        peak = np.abs(pcm.astype(np.int32)).max(axis=0) / 32768.0
        with self._peak_lock:
            np.maximum(self._peak, peak, out=self._peak)
        monitor, sender = self.monitor, self.sender
        if monitor:
            monitor.push(data)
        if sender:
            sender.push(data)

    def take_peaks(self) -> np.ndarray:
        with self._peak_lock:
            peaks = self._peak.copy()
            self._peak[:] = 0
        return peaks

    def snapshot(self) -> dict[str, tuple[str, str]]:
        out = {}
        for key, comp in (("source", self.source), ("monitor", self.monitor), ("sender", self.sender)):
            out[key] = comp.status() if comp else ("off", "")
        return out


# --------------------------------------------------------------------------
# GUI
# --------------------------------------------------------------------------
GOOD = {"receiving", "capturing", "playing", "sending"}
WAIT = {"connecting", "reconnecting", "buffering", "no audio"}
COLORS = {"good": "#2e9d4a", "wait": "#e0a000", "bad": "#d33b2f", "off": "#9a9a9a"}
DEFAULT_DEVICE = "System default"


def state_color(state: str) -> str:
    if state in GOOD:
        return COLORS["good"]
    if state in WAIT:
        return COLORS["wait"]
    if state == "error":
        return COLORS["bad"]
    return COLORS["off"]


class App:
    POLL_MS = 50

    def __init__(self, root, config_path: Path):
        self.root = root
        self.config_path = config_path
        self.cfg = load_config(config_path)
        self.engine: Engine | None = None
        self.logq: queue.Queue[str] = queue.Queue()
        self.devices: list[Device] = []
        self.meter_db = [-90.0] * CHANNELS
        self.tick = 0

        root.title(APP_NAME)
        root.minsize(900, 660)
        style = ttk.Style(root)
        if sys.platform.startswith("linux"):
            style.theme_use("clam")
        style.configure("Hint.TLabel", foreground="#777")
        style.configure("Big.TButton", padding=(14, 6))

        self._build()
        self._fill_widgets()
        self.refresh_devices(reinit=False)
        self.check_ffmpeg()
        if SD_ERROR:
            self.log(f"Audio devices unavailable ({SD_ERROR}). SRT-to-SRT still works with monitoring off.")
        root.protocol("WM_DELETE_WINDOW", self.on_close)
        root.after(self.POLL_MS, self._poll)

    # -- thread-safe log -------------------------------------------------
    def log(self, msg: str):
        self.logq.put(f"{time.strftime('%H:%M:%S')}  {mask_secrets(msg)}")

    # -- layout ------------------------------------------------------------
    def _build(self):
        r = self.root
        r.columnconfigure(0, weight=1, uniform="col")
        r.columnconfigure(1, weight=1, uniform="col")
        r.rowconfigure(5, weight=1)
        P = dict(padx=8, pady=4)

        # toolbar
        bar = ttk.Frame(r, padding=(8, 8, 8, 2))
        bar.grid(row=0, column=0, columnspan=2, sticky="ew")
        self.btn_start = ttk.Button(bar, text="▶  Start", style="Big.TButton", command=self.start)
        self.btn_start.pack(side="left")
        self.btn_stop = ttk.Button(bar, text="■  Stop", style="Big.TButton", command=self.stop, state="disabled")
        self.btn_stop.pack(side="left", padx=(6, 0))
        ttk.Button(bar, text="⟳ Devices", command=self.refresh_devices).pack(side="right")
        self.v_api = tk.StringVar()
        self.cb_api = ttk.Combobox(bar, textvariable=self.v_api, state="readonly", width=18)
        self.cb_api.pack(side="right", padx=6)
        self.cb_api.bind("<<ComboboxSelected>>", lambda e: self._fill_device_lists())
        ttk.Label(bar, text="Audio API").pack(side="right")

        # source
        src = ttk.LabelFrame(r, text="Source", padding=8)
        src.grid(row=1, column=0, sticky="nsew", **P)
        src.columnconfigure(0, weight=1)
        self.v_source = tk.StringVar()
        radios = ttk.Frame(src)
        radios.grid(row=0, column=0, sticky="w")
        ttk.Radiobutton(radios, text="SRT stream", value="srt", variable=self.v_source,
                        command=self._update_enabled).pack(side="left")
        ttk.Radiobutton(radios, text="Local input device", value="device", variable=self.v_source,
                        command=self._update_enabled).pack(side="left", padx=(12, 0))
        self.in_panel, self.in_vars = self._srt_panel(src)
        self.in_panel.grid(row=1, column=0, sticky="ew", pady=(6, 0))
        self.v_lowlat = tk.BooleanVar()
        self.chk_lowlat = ttk.Checkbutton(src, text="Low-latency start (smaller probe)", variable=self.v_lowlat)
        self.chk_lowlat.grid(row=2, column=0, sticky="w", pady=(4, 0))
        dev_row = ttk.Frame(src)
        dev_row.grid(row=3, column=0, sticky="ew", pady=(8, 0))
        dev_row.columnconfigure(1, weight=1)
        ttk.Label(dev_row, text="Input device").grid(row=0, column=0, sticky="w", padx=(0, 6))
        self.v_in_dev = tk.StringVar()
        self.cb_in = ttk.Combobox(dev_row, textvariable=self.v_in_dev, state="readonly")
        self.cb_in.grid(row=0, column=1, sticky="ew")

        # destination
        dst = ttk.LabelFrame(r, text="SRT output", padding=8)
        dst.grid(row=1, column=1, sticky="nsew", **P)
        dst.columnconfigure(0, weight=1)
        self.v_send = tk.BooleanVar()
        ttk.Checkbutton(dst, text="Send over SRT", variable=self.v_send,
                        command=self._update_enabled).grid(row=0, column=0, sticky="w")
        self.out_panel, self.out_vars = self._srt_panel(dst)
        self.out_panel.grid(row=1, column=0, sticky="ew", pady=(6, 0))
        enc = ttk.Frame(dst)
        enc.grid(row=2, column=0, sticky="w", pady=(8, 0))
        ttk.Label(enc, text="Codec").pack(side="left")
        self.v_codec = tk.StringVar()
        self.cb_codec = ttk.Combobox(enc, textvariable=self.v_codec, values=list(CODECS),
                                     state="readonly", width=17)
        self.cb_codec.pack(side="left", padx=(6, 12))
        self.cb_codec.bind("<<ComboboxSelected>>", lambda e: self._update_enabled())
        ttk.Label(enc, text="Bitrate").pack(side="left")
        self.v_bitrate = tk.StringVar()
        self.cb_bitrate = ttk.Combobox(enc, textvariable=self.v_bitrate, values=BITRATES, width=5)
        self.cb_bitrate.pack(side="left", padx=6)
        ttk.Label(enc, text="kbit/s").pack(side="left")
        self.v_keepalive = tk.BooleanVar()
        ttk.Checkbutton(dst, text="Send silence while the source is down (keeps the link up)",
                        variable=self.v_keepalive).grid(row=3, column=0, sticky="w", pady=(4, 0))

        # monitor
        mon = ttk.LabelFrame(r, text="Monitor", padding=8)
        mon.grid(row=2, column=0, columnspan=2, sticky="ew", **P)
        mon.columnconfigure(2, weight=1)
        self.v_monitor = tk.BooleanVar()
        ttk.Checkbutton(mon, text="Monitor", variable=self.v_monitor,
                        command=self._monitor_changed).grid(row=0, column=0, sticky="w")
        ttk.Label(mon, text="Output").grid(row=0, column=1, padx=(12, 6))
        self.v_out_dev = tk.StringVar()
        self.cb_out = ttk.Combobox(mon, textvariable=self.v_out_dev, state="readonly")
        self.cb_out.grid(row=0, column=2, sticky="ew")
        self.cb_out.bind("<<ComboboxSelected>>", lambda e: self._monitor_changed())
        ttk.Label(mon, text="Volume").grid(row=0, column=3, padx=(12, 4))
        self.v_volume = tk.DoubleVar()
        ttk.Scale(mon, from_=0.0, to=1.5, variable=self.v_volume, length=140,
                  command=lambda _v: self._volume_changed()).grid(row=0, column=4)
        self.v_mute = tk.BooleanVar()
        ttk.Checkbutton(mon, text="Mute", variable=self.v_mute,
                        command=self._volume_changed).grid(row=0, column=5, padx=(8, 0))
        ttk.Label(mon, text="Buffer").grid(row=0, column=6, padx=(12, 4))
        self.v_buffer = tk.StringVar()
        sp = ttk.Spinbox(mon, textvariable=self.v_buffer, from_=20, to=1000, increment=10, width=5,
                         command=self._monitor_changed)
        sp.grid(row=0, column=7)
        sp.bind("<Return>", lambda e: self._monitor_changed())
        ttk.Label(mon, text="ms").grid(row=0, column=8, padx=(4, 0))

        # meters + status
        st = ttk.LabelFrame(r, text="Levels & status", padding=8)
        st.grid(row=3, column=0, columnspan=2, sticky="ew", **P)
        st.columnconfigure(0, weight=1)
        self.meter = tk.Canvas(st, height=44, highlightthickness=0, background="#1e1e1e")
        self.meter.grid(row=0, column=0, sticky="ew")
        lights = ttk.Frame(st)
        lights.grid(row=1, column=0, sticky="ew", pady=(8, 0))
        self.lights = {}
        for i, (key, title) in enumerate((("source", "Source"), ("monitor", "Monitor"), ("sender", "SRT out"))):
            lights.columnconfigure(i, weight=1)
            cell = ttk.Frame(lights)
            cell.grid(row=0, column=i, sticky="w")
            dot = tk.Canvas(cell, width=14, height=14, highlightthickness=0)
            dot.create_oval(2, 2, 12, 12, fill=COLORS["off"], outline="", tags="dot")
            dot.pack(side="left")
            text = tk.StringVar(value=f"{title}: off")
            ttk.Label(cell, textvariable=text).pack(side="left", padx=(4, 0))
            self.lights[key] = (dot, text, title)

        # ffmpeg + settings
        ff = ttk.Frame(r, padding=(8, 0, 8, 0))
        ff.grid(row=4, column=0, columnspan=2, sticky="ew")
        ff.columnconfigure(1, weight=1)
        ttk.Label(ff, text="ffmpeg").grid(row=0, column=0, padx=(0, 6))
        self.v_ffmpeg = tk.StringVar()
        ttk.Entry(ff, textvariable=self.v_ffmpeg).grid(row=0, column=1, sticky="ew")
        ttk.Button(ff, text="Browse…", command=self._browse_ffmpeg).grid(row=0, column=2, padx=4)
        ttk.Button(ff, text="Check", command=self.check_ffmpeg).grid(row=0, column=3)
        self.v_ffinfo = tk.StringVar()
        ttk.Label(ff, textvariable=self.v_ffinfo, style="Hint.TLabel").grid(row=0, column=4, padx=8)
        self.v_savepass = tk.BooleanVar()
        ttk.Checkbutton(ff, text="Save passphrases", variable=self.v_savepass).grid(row=0, column=5)

        # log
        lg = ttk.LabelFrame(r, text="Log", padding=4)
        lg.grid(row=5, column=0, columnspan=2, sticky="nsew", **P)
        lg.columnconfigure(0, weight=1)
        lg.rowconfigure(0, weight=1)
        self.txt = tk.Text(lg, height=8, wrap="word", state="disabled", font="TkFixedFont")
        self.txt.grid(row=0, column=0, sticky="nsew")
        sb = ttk.Scrollbar(lg, command=self.txt.yview)
        sb.grid(row=0, column=1, sticky="ns")
        self.txt.configure(yscrollcommand=sb.set)

    def _srt_panel(self, parent):
        f = ttk.Frame(parent)
        f.columnconfigure(1, weight=1)
        v = {k: tk.StringVar() for k in ("mode", "host", "port", "latency_ms", "passphrase", "streamid")}
        ttk.Label(f, text="Mode").grid(row=0, column=0, sticky="w")
        ttk.Combobox(f, textvariable=v["mode"], values=SRT_MODES, state="readonly", width=11
                     ).grid(row=0, column=1, sticky="w", pady=2)
        ttk.Label(f, text="Latency ms").grid(row=0, column=2, sticky="e", padx=(8, 4))
        ttk.Spinbox(f, textvariable=v["latency_ms"], from_=20, to=8000, increment=10, width=7
                    ).grid(row=0, column=3, sticky="w")
        ttk.Label(f, text="Host").grid(row=1, column=0, sticky="w")
        ttk.Entry(f, textvariable=v["host"]).grid(row=1, column=1, sticky="ew", pady=2)
        ttk.Label(f, text="Port").grid(row=1, column=2, sticky="e", padx=(8, 4))
        ttk.Spinbox(f, textvariable=v["port"], from_=1, to=65535, width=7).grid(row=1, column=3, sticky="w")
        ttk.Label(f, text="Passphrase").grid(row=2, column=0, sticky="w", padx=(0, 6))
        ttk.Entry(f, textvariable=v["passphrase"], show="•").grid(row=2, column=1, sticky="ew", pady=2)
        ttk.Label(f, text="Stream ID").grid(row=2, column=2, sticky="e", padx=(8, 4))
        ttk.Entry(f, textvariable=v["streamid"], width=12).grid(row=2, column=3, sticky="w")
        hint = tk.StringVar()
        ttk.Label(f, textvariable=hint, style="Hint.TLabel").grid(row=3, column=0, columnspan=4, sticky="w")
        url = tk.StringVar()
        ttk.Entry(f, textvariable=url, state="readonly").grid(row=4, column=0, columnspan=4, sticky="ew")

        def update(*_):
            hint.set(MODE_HINTS.get(v["mode"].get(), ""))
            try:
                ep = self._endpoint(v)
                ep.validate()
                url.set(ep.url(mask=True))
            except (ValueError, TypeError) as exc:
                url.set(f"⚠ {exc}")
        for var in v.values():
            var.trace_add("write", update)
        f.vars_widgets = [w for w in f.winfo_children() if not isinstance(w, ttk.Label)]
        return f, v

    # -- config <-> widgets ----------------------------------------------
    @staticmethod
    def _endpoint(v) -> SrtEndpoint:
        def num(name, key):
            try:
                return int(str(v[key].get()).strip() or 0)
            except ValueError:
                raise ValueError(f"{name} must be a number") from None
        return SrtEndpoint(mode=v["mode"].get(), host=v["host"].get().strip(),
                           port=num("port", "port"), latency_ms=num("latency", "latency_ms"),
                           passphrase=v["passphrase"].get(), streamid=v["streamid"].get().strip())

    @staticmethod
    def _set_endpoint(v, ep: SrtEndpoint):
        for key in v:
            v[key].set(str(getattr(ep, key)))

    def _fill_widgets(self):
        c = self.cfg
        self.v_source.set(c.source)
        self._set_endpoint(self.in_vars, c.srt_in)
        self._set_endpoint(self.out_vars, c.srt_out)
        self.v_lowlat.set(c.low_latency_input)
        self.v_send.set(c.send_enabled)
        self.v_codec.set(c.codec if c.codec in CODECS else "AAC")
        self.v_bitrate.set(str(c.bitrate_k))
        self.v_keepalive.set(c.keepalive_silence)
        self.v_monitor.set(c.monitor_enabled)
        self.v_volume.set(c.monitor_volume)
        self.v_mute.set(c.monitor_muted)
        self.v_buffer.set(str(c.monitor_buffer_ms))
        self.v_ffmpeg.set(c.ffmpeg)
        self.v_savepass.set(c.save_passphrases)
        self._update_enabled()

    def _collect(self) -> AppConfig:
        def as_int(var, name, lo, hi):
            try:
                value = int(float(var.get()))
            except ValueError:
                raise ValueError(f"{name} must be a number") from None
            if not lo <= value <= hi:
                raise ValueError(f"{name} must be {lo}-{hi}")
            return value
        return AppConfig(
            ffmpeg=self.v_ffmpeg.get().strip() or "ffmpeg",
            source=self.v_source.get(),
            srt_in=self._endpoint(self.in_vars),
            low_latency_input=self.v_lowlat.get(),
            host_api=self.v_api.get(),
            input_device=self._device_value(self.v_in_dev),
            monitor_enabled=self.v_monitor.get(),
            monitor_device=self._device_value(self.v_out_dev),
            monitor_volume=round(self.v_volume.get(), 3),
            monitor_muted=self.v_mute.get(),
            monitor_buffer_ms=as_int(self.v_buffer, "Monitor buffer", 20, 1000),
            send_enabled=self.v_send.get(),
            srt_out=self._endpoint(self.out_vars),
            codec=self.v_codec.get(),
            bitrate_k=as_int(self.v_bitrate, "Bitrate", 16, 512),
            keepalive_silence=self.v_keepalive.get(),
            save_passphrases=self.v_savepass.get(),
        )

    @staticmethod
    def _device_value(var) -> str:
        value = var.get()
        return "" if value == DEFAULT_DEVICE else value

    def _update_enabled(self):
        src_srt = self.v_source.get() == "srt"
        for w in self.in_panel.vars_widgets:
            self._enable(w, src_srt)
        self._enable(self.chk_lowlat, src_srt)
        self._enable(self.cb_in, not src_srt)
        for w in self.out_panel.vars_widgets:
            self._enable(w, self.v_send.get())
        self._enable(self.cb_bitrate, self.v_codec.get() not in LOSSLESS)

    @staticmethod
    def _enable(widget, on: bool):
        try:
            if isinstance(widget, ttk.Combobox):
                # remember whether this combobox is normally "readonly" or editable
                if not hasattr(widget, "on_state"):
                    widget.on_state = str(widget.cget("state")) or "normal"
                widget.configure(state=widget.on_state if on else "disabled")
            elif isinstance(widget, ttk.Entry) and str(widget.cget("state")) == "readonly":
                return  # URL preview stays selectable
            else:
                widget.state(["!disabled"] if on else ["disabled"])
        except tk.TclError:
            pass

    # -- devices -----------------------------------------------------------
    def refresh_devices(self, reinit: bool = True):
        if sd is not None and reinit:
            if self.engine and self.engine.running:
                self.log("Stop before rescanning devices (PortAudio must restart to see new ones)")
            else:
                try:  # PortAudio only sees newly plugged devices after a restart
                    sd._terminate()
                    sd._initialize()
                except Exception as exc:
                    self.log(f"Device rescan failed: {exc}")
        try:
            self.devices = list_devices()
        except Exception as exc:
            self.devices = []
            self.log(f"Listing devices failed: {exc}")
        apis = sorted({d.api for d in self.devices})
        self.cb_api.configure(values=["All", *apis])
        wanted = self.v_api.get() or self.cfg.host_api
        self.v_api.set(wanted if wanted in apis or wanted == "All" else default_host_api(self.devices))
        self._fill_device_lists()
        if reinit:
            self.log(f"Found {len(self.devices)} audio devices")

    def _fill_device_lists(self):
        api = self.v_api.get()
        devs = [d for d in self.devices if api in ("All", "") or d.api == api]
        for cb, var, kind, saved in ((self.cb_in, self.v_in_dev, "in", self.cfg.input_device),
                                     (self.cb_out, self.v_out_dev, "out", self.cfg.monitor_device)):
            labels = [d.label for d in devs if (d.max_in if kind == "in" else d.max_out) > 0]
            cb.configure(values=[DEFAULT_DEVICE, *labels])
            current = var.get() or saved or DEFAULT_DEVICE
            var.set(current if current in labels or current == DEFAULT_DEVICE else DEFAULT_DEVICE)

    # -- ffmpeg ------------------------------------------------------------
    def _browse_ffmpeg(self):
        path = filedialog.askopenfilename(title="Select the ffmpeg binary")
        if path:
            self.v_ffmpeg.set(path)
            self.check_ffmpeg()

    def check_ffmpeg(self):
        path = find_ffmpeg(self.v_ffmpeg.get().strip() or "ffmpeg")
        if not path:
            self.v_ffinfo.set("✗ not found")
            self.log("ffmpeg not found. Install it or set the path.")
            return
        info = ffmpeg_info(path)
        if not info["ok"]:
            self.v_ffinfo.set("✗ won't run")
            self.log(f"ffmpeg check failed: {info['error']}")
            return
        srt = "SRT ✓" if info["srt"] else "SRT ✗"
        self.v_ffinfo.set(f"{info['version'][:18]} · {srt}")
        if not info["srt"]:
            self.log("This ffmpeg build has no SRT support. Use a build with libsrt "
                     "(gyan.dev/BtbN on Windows, Homebrew on macOS, distro ffmpeg on most Linux).")
        missing = [name for name, args in CODECS.items() if args[1] not in info["encoders"]]
        if missing:
            self.log(f"Codecs not in this ffmpeg build: {', '.join(missing)}")

    # -- run control -------------------------------------------------------
    def start(self):
        try:
            cfg = self._collect()
        except ValueError as exc:
            messagebox.showerror(APP_NAME, str(exc))
            return
        if self.engine:
            self.engine.stop()
            self.log("Restarting with new settings")
        self.cfg = cfg
        try:
            save_config(cfg, self.config_path)
        except OSError as exc:
            self.log(f"Could not save settings: {exc}")
        self.engine = Engine(cfg, self.log)
        try:
            self.engine.start()
        except Exception as exc:
            self.engine = None
            self._set_running(False)
            self.log(f"Start failed: {exc}")
            messagebox.showerror(APP_NAME, str(exc))
            return
        self._set_running(True)
        self.log("Started")

    def stop(self):
        if self.engine:
            self.engine.stop()
            self.engine = None
            self.log("Stopped")
        self._set_running(False)

    def _set_running(self, running: bool):
        self.btn_start.configure(text="↻  Restart" if running else "▶  Start")
        self.btn_stop.state(["!disabled"] if running else ["disabled"])

    def _monitor_changed(self):
        if not (self.engine and self.engine.running):
            return
        try:
            buffer_ms = max(20, min(1000, int(float(self.v_buffer.get()))))
            self.engine.set_monitor(self.v_monitor.get(), self._device_value(self.v_out_dev), buffer_ms)
        except Exception as exc:
            self.log(f"Monitor: {exc}")

    def _volume_changed(self):
        if self.engine:
            self.engine.set_volume(round(self.v_volume.get(), 3), self.v_mute.get())

    # -- periodic UI update ------------------------------------------------
    def _poll(self):
        lines = []
        while True:
            try:
                lines.append(self.logq.get_nowait())
            except queue.Empty:
                break
        if lines:
            self.txt.configure(state="normal")
            self.txt.insert("end", "\n".join(lines) + "\n")
            excess = int(self.txt.index("end-1c").split(".")[0]) - 800
            if excess > 0:
                self.txt.delete("1.0", f"{excess}.0")
            self.txt.configure(state="disabled")
            self.txt.see("end")

        peaks = self.engine.take_peaks() if self.engine else np.zeros(CHANNELS)
        for i in range(CHANNELS):
            db = 20 * math.log10(max(float(peaks[i]), 1e-5))
            self.meter_db[i] = max(db, self.meter_db[i] - 1.2)   # ~24 dB/s fall-back
        self._draw_meters()

        self.tick += 1
        if self.tick % 5 == 0:
            snap = self.engine.snapshot() if self.engine else {}
            for key, (dot, text, title) in self.lights.items():
                state, detail = snap.get(key, ("off", ""))
                dot.itemconfigure("dot", fill=state_color(state))
                text.set(f"{title}: {state}" + (f" — {detail}" if detail else ""))
        self.root.after(self.POLL_MS, self._poll)

    def _draw_meters(self):
        c = self.meter
        c.delete("all")
        w, h = max(c.winfo_width(), 200), max(c.winfo_height(), 44)
        left, right = 22, w - 90
        span = right - left
        bar_h = (h - 12) // 2

        def x(db):
            return left + span * (max(-60.0, min(0.0, db)) + 60) / 60

        zones = ((-60, -18, "#1f5130", "#36c35c"), (-18, -6, "#5a4c12", "#f2c230"), (-6, 0, "#5c1e1a", "#ef4d3c"))
        for ch, name in enumerate("LR"):
            y0 = 4 + ch * (bar_h + 4)
            y1 = y0 + bar_h
            c.create_text(10, (y0 + y1) / 2, text=name, fill="#ccc", font="TkDefaultFont")
            level = self.meter_db[ch]
            for lo, hi, dim, lit in zones:
                c.create_rectangle(x(lo), y0, x(hi), y1, fill=dim, width=0)
                if level > lo:
                    c.create_rectangle(x(lo), y0, x(min(level, hi)), y1, fill=lit, width=0)
            label = "-inf" if level <= -60 else f"{level:5.1f} dB"
            c.create_text(right + 8, (y0 + y1) / 2, text=label, fill="#ddd", anchor="w",
                          font="TkFixedFont")
        for db in (-48, -36, -24, -18, -12, -6, -3):
            c.create_line(x(db), 4, x(db), h - 4, fill="#000", width=1)

    def on_close(self):
        if self.engine:
            self.engine.stop()
        try:
            save_config(self._collect(), self.config_path)
        except (ValueError, OSError):
            pass
        self.root.destroy()


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------
def print_devices():
    if sd is None:
        print(f"Audio devices unavailable: {SD_ERROR}")
        return
    default_in, default_out = sd.default.device
    print(f"{'#':>3}  {'in':>3} {'out':>3}  device")
    for d in list_devices():
        mark = " (default in)" if d.index == default_in else ""
        mark += " (default out)" if d.index == default_out else ""
        print(f"{d.index:>3}  {d.max_in:>3} {d.max_out:>3}  {d.label}{mark}")


def run_headless(config_path: Path):
    if not Path(config_path).is_file():
        sys.exit(f"No settings file at {config_path}. Start the GUI once and press Start, "
                 "or pass --config with a saved file.")
    cfg = load_config(config_path)
    stop = threading.Event()
    out_lock = threading.Lock()

    def log(msg):
        with out_lock:  # one write per line so threads don't interleave
            sys.stdout.write(f"{time.strftime('%H:%M:%S')}  {mask_secrets(msg)}\n")
            sys.stdout.flush()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, lambda *_: stop.set())
        except (ValueError, OSError):
            pass
    engine = Engine(cfg, log)
    try:
        engine.start()
    except Exception as exc:
        log(f"Start failed: {exc}")
        sys.exit(1)
    log(f"Running from {config_path}. Ctrl+C to stop.")
    last, n = "", 0
    while not stop.wait(1.0):
        n += 1
        line = " | ".join(f"{k}: {s}" + (f" ({d})" if d else "") for k, (s, d) in engine.snapshot().items())
        if line != last:
            log(line)
            last = line
        peaks = engine.take_peaks()
        if n % 30 == 0:
            log("levels " + "  ".join(f"{20 * math.log10(max(float(p), 1e-5)):.1f} dB" for p in peaks))
    engine.stop()
    log("Stopped")


def main():
    ap = argparse.ArgumentParser(description=APP_NAME)
    ap.add_argument("--config", type=Path, default=DEFAULT_CONFIG,
                    help=f"settings file (default {DEFAULT_CONFIG})")
    ap.add_argument("--headless", action="store_true", help="run the saved settings without a GUI")
    ap.add_argument("--list-devices", action="store_true", help="print audio devices and exit")
    args = ap.parse_args()
    if args.list_devices:
        print_devices()
        return
    if args.headless:
        run_headless(args.config)
        return
    if tk is None:
        sys.exit("tkinter isn't available in this Python. Install it (python3-tk / python-tk) "
                 "or use --headless.")
    root = tk.Tk()
    App(root, args.config)
    root.mainloop()


if __name__ == "__main__":
    main()
