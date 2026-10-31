#!/usr/bin/env python3
"""End-to-end check: tone pulses over SRT -> srt_audio_bridge (headless) -> SRT -> receiver.

Needs ffmpeg with SRT and numpy. No audio devices (monitoring is off).

  python tests/e2e_relay.py                                     # bridge calls the destination
  python tests/e2e_relay.py --out-mode listener --join-after 5  # bridge listens, receiver joins late

Passes when every pulse arrives intact (no dropouts) within --max-latency.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
RATE = 48_000
BLOCK = 480            # 10 ms
PERIOD = 2.0           # seconds between pulse starts
PULSE = 0.3            # pulse length in seconds
LOUD = 3000            # RMS threshold for "tone present"


def ffmpeg_receiver(ffmpeg: str, mode: str, port: int) -> subprocess.Popen:
    host = "0.0.0.0" if mode == "listener" else "127.0.0.1"
    return subprocess.Popen(
        [ffmpeg, "-hide_banner", "-loglevel", "error", "-fflags", "nobuffer",
         "-probesize", "32768", "-analyzeduration", "250000",
         "-i", f"srt://{host}:{port}?mode={mode}&latency=120000",
         "-f", "s16le", "-ac", "1", "-ar", str(RATE), "pipe:1"],
        stdout=subprocess.PIPE)


def feed_pulses(proc: subprocess.Popen, t0: float):
    """Write 1 kHz pulses paced to the wall clock, like a live source."""
    n = 0
    while True:
        t = (n + np.arange(BLOCK)) / RATE
        tone = 0.5 * np.sin(2 * np.pi * 1000 * t) * ((t % PERIOD) < PULSE)
        pcm = (tone * 32767).astype(np.int16)
        try:
            proc.stdin.write(np.repeat(pcm[:, None], 2, axis=1).tobytes())
        except (BrokenPipeError, OSError, ValueError):
            return
        n += BLOCK
        delay = t0 + n / RATE - time.monotonic()
        if delay > 0:
            time.sleep(delay)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out-mode", choices=("caller", "listener"), default="caller",
                    help="SRT mode of the bridge's output")
    ap.add_argument("--join-after", type=float, default=0.0,
                    help="with --out-mode listener: seconds before the receiver connects")
    ap.add_argument("--seconds", type=float, default=10.0, help="how long to listen")
    ap.add_argument("--codec", default="AAC", help="AAC, MP2, Opus or 'PCM (SMPTE 302M)'")
    ap.add_argument("--port", type=int, default=9700, help="bridge input port; output uses port+1")
    ap.add_argument("--max-latency", type=float, default=0.8, help="seconds")
    ap.add_argument("--ffmpeg", default="ffmpeg")
    args = ap.parse_args()

    in_port, out_port = args.port, args.port + 1
    rcv_mode = "listener" if args.out_mode == "caller" else "caller"
    work = Path(tempfile.mkdtemp(prefix="srt-bridge-test-"))
    cfg_path, log_path = work / "config.json", work / "bridge.log"
    cfg_path.write_text(json.dumps({
        "ffmpeg": args.ffmpeg, "source": "srt",
        "srt_in": {"mode": "listener", "port": in_port, "latency_ms": 120},
        "monitor_enabled": False, "send_enabled": True,
        "srt_out": {"mode": args.out_mode, "host": "127.0.0.1", "port": out_port, "latency_ms": 120},
        "codec": args.codec, "bitrate_k": 192,
    }))

    procs: list[subprocess.Popen] = []
    onsets: list[tuple[float, float]] = []      # (arrival time since t0, pulse length)
    try:
        receiver = None
        if rcv_mode == "listener":
            receiver = ffmpeg_receiver(args.ffmpeg, rcv_mode, out_port)
            procs.append(receiver)
        with open(log_path, "w") as log:
            procs.append(subprocess.Popen(
                [sys.executable, str(ROOT / "srt_audio_bridge.py"), "--headless", "--config", str(cfg_path)],
                stdout=log, stderr=subprocess.STDOUT))
        time.sleep(1.5)
        encoder = subprocess.Popen(
            [args.ffmpeg, "-hide_banner", "-loglevel", "error", "-probesize", "32", "-analyzeduration", "0",
             "-f", "s16le", "-ar", str(RATE), "-ac", "2", "-i", "pipe:0",
             "-c:a", "aac", "-b:a", "160k", "-f", "mpegts", "-flush_packets", "1", "-muxdelay", "0",
             f"srt://127.0.0.1:{in_port}?mode=caller&latency=120000&pkt_size=1316"],
            stdin=subprocess.PIPE, bufsize=0)
        procs.append(encoder)
        t0 = time.monotonic()
        threading.Thread(target=feed_pulses, args=(encoder, t0), daemon=True).start()
        if receiver is None:
            time.sleep(args.join_after)
            receiver = ffmpeg_receiver(args.ffmpeg, rcv_mode, out_port)
            procs.append(receiver)
        joined = time.monotonic() - t0

        start = None
        end = t0 + joined + args.seconds
        while time.monotonic() < end:
            buf = receiver.stdout.read(BLOCK * 2)
            if not buf:
                break
            now = time.monotonic() - t0
            x = np.frombuffer(buf, np.int16).astype(np.float64)
            loud = np.sqrt((x ** 2).mean()) > LOUD
            if loud and start is None:
                start, length = now, 0
            if start is not None:
                if loud:
                    length += len(x) / RATE
                else:
                    onsets.append((start, length))
                    start = None
    finally:
        for p in reversed(procs):
            p.terminate()
        for p in procs:
            try:
                p.wait(timeout=3)
            except subprocess.TimeoutExpired:
                p.kill()

    # Skip pulses in the receiver's own start-up burst (its probe makes them arrive bunched up).
    settled = [(t, n) for t, n in onsets if t > joined + 2.0]
    latencies = [t % PERIOD for t, _ in settled]
    lengths = [n for _, n in settled]
    expected = int(args.seconds - 2.0 - args.max_latency) // int(PERIOD)
    print(f"SRT out {args.out_mode}, codec {args.codec}, receiver joined at {joined:.1f} s")
    print("latency per pulse:", " ".join(f"{l * 1000:.0f} ms" for l in latencies) or "none")
    print("pulse length:     ", " ".join(f"{n * 1000:.0f} ms" for n in lengths) or "none")

    problems = []
    if len(settled) < max(1, expected):
        problems.append(f"only {len(settled)} pulses arrived, expected at least {expected}")
    if any(l > args.max_latency for l in latencies):
        problems.append(f"latency above {args.max_latency * 1000:.0f} ms")
    if any(abs(n - PULSE) > 0.04 for n in lengths):
        problems.append("a pulse was cut short or split (dropout)")
    if problems:
        print("FAIL: " + "; ".join(problems))
        print(f"bridge log: {log_path}")
        return 1
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
