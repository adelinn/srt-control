package bridge

import (
	"bufio"
	"fmt"
	"strings"
	"sync"
	"time"
)

// srtSender runs ffmpeg to encode PCM from stdin and send MPEG-TS over SRT.
// It runs independently of the source, so a dead destination never stops monitoring.
type srtSender struct {
	statusBox
	cfg    Config
	ffmpeg string
	log    func(string)
	seen   errorDedupe
	ctl    stopper
	q      chan []byte // 200 ms of 10 ms blocks

	mu      sync.Mutex
	proc    *ffproc
	bitrate string
}

func newSender(cfg Config, ffmpeg string, log func(string)) *srtSender {
	return &srtSender{cfg: cfg, ffmpeg: ffmpeg, log: log, ctl: newStopper(), q: make(chan []byte, 20)}
}

func (s *srtSender) command() []string {
	cmd := []string{s.ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "warning",
		"-nostats", "-progress", "pipe:1",
		// tiny probe: by default ffmpeg buffers seconds of raw PCM before it
		// starts, and that buffer becomes permanent latency downstream
		"-probesize", "32", "-analyzeduration", "0",
		"-f", "s16le", "-ar", fmt.Sprint(Rate), "-ac", fmt.Sprint(Channels), "-i", "pipe:0"}
	cmd = append(cmd, codecArgs[s.cfg.Codec]...)
	if s.cfg.Codec == "Opus" {
		// 10 ms frames instead of 20, and no look-ahead for speech/music analysis
		cmd = append(cmd, "-application", "lowdelay", "-frame_duration", "10")
	}
	if !IsLossless(s.cfg.Codec) {
		cmd = append(cmd, "-b:a", fmt.Sprintf("%dk", s.cfg.BitrateK))
	}
	return append(cmd, "-f", "mpegts", "-flush_packets", "1", "-muxdelay", "0", "-muxpreload", "0",
		// one PES packet per audio frame: by default the muxer gathers ~2.9 kB
		// first, which is ~120 ms of audio at 192 kbit/s
		"-pes_payload_size", "0",
		s.cfg.SrtOut.URL(nil, false))
}

func (s *srtSender) start() { go s.run() }

func (s *srtSender) stop() {
	s.ctl.signal()
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		p.stop()
	}
	select {
	case <-s.ctl.done:
	case <-time.After(3 * time.Second):
	}
}

func (s *srtSender) status() (string, string) { return s.get() }

// push queues a block, dropping the oldest one rather than building latency.
func (s *srtSender) push(data []byte) {
	for {
		select {
		case s.q <- data:
			return
		default:
		}
		select {
		case <-s.q:
		default:
		}
	}
}

func (s *srtSender) clearQueue() {
	for {
		select {
		case <-s.q:
		default:
			return
		}
	}
}

// progress reads ffmpeg's -progress output. The first "progress=" block means
// the SRT output is open.
func (s *srtSender) progress(p *ffproc, flowing chan struct{}) {
	sc := bufio.NewScanner(p.stdout)
	open := false
	for sc.Scan() {
		key, value, _ := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		switch key {
		case "bitrate":
			s.mu.Lock()
			if strings.Contains(value, "N/A") {
				s.bitrate = ""
			} else {
				s.bitrate = strings.Replace(strings.TrimSpace(value), "kbits/s", " kbit/s", 1)
			}
			s.mu.Unlock()
		case "progress":
			if !open {
				open = true
				close(flowing)
				s.log("SRT out: connected")
			}
			s.mu.Lock()
			br, current := s.bitrate, s.proc == p
			s.mu.Unlock()
			if current && p.running() {
				s.set("sending", br)
			}
		}
	}
}

// waitUntilOpen holds real audio back until ffmpeg reports that the SRT output
// is open, so nothing stale piles up in the pipe while a listener waits for its
// receiver (that backlog would become latency).
func (s *srtSender) waitUntilOpen(p *ffproc, flowing chan struct{}) bool {
	if _, err := p.stdin.Write(make([]byte, 5*BlockBytes)); err != nil { // 50 ms silence lets ffmpeg open its input
		return false
	}
	t0 := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-flowing:
			return true
		case <-s.ctl.ch:
			return false
		case <-p.exited:
			return false
		case <-tick.C:
			// very old ffmpeg builds only report once fed; a caller connects fast anyway
			if mode, _, _, _ := s.cfg.SrtOut.Target(); mode != "listener" && time.Since(t0) > 3*time.Second {
				return true
			}
		}
	}
}

// pump feeds queued audio to ffmpeg. While the source is down it sends silence
// so the SRT link stays up.
func (s *srtSender) pump(p *ffproc) {
	last := time.Now()
	idle := false
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		var chunk []byte
		timer.Reset(100 * time.Millisecond)
		select {
		case <-s.ctl.ch:
			return
		case <-p.exited:
			return
		case chunk = <-s.q:
			idle = false
		case <-timer.C:
			gap := time.Since(last)
			if !s.cfg.KeepaliveSilence || (!idle && gap < keepaliveAfter) {
				continue
			}
			idle = true // source is down: keep the stream alive with silence
			chunk = make([]byte, int(min(gap, time.Second).Seconds()*Rate)*FrameBytes)
		}
		if _, err := p.stdin.Write(chunk); err != nil {
			return
		}
		last = time.Now()
	}
}

func (s *srtSender) run() {
	defer close(s.ctl.done)
	mode, host, port, _ := s.cfg.SrtOut.Target() // validated before start
	backoff := time.Second
	first := true
	for !s.ctl.stopped() {
		flowing := make(chan struct{})
		s.clearQueue()
		s.mu.Lock()
		s.bitrate = ""
		s.mu.Unlock()
		if mode == "listener" {
			s.set("connecting", "waiting for a receiver")
		} else {
			s.set("connecting", fmt.Sprintf("connecting to %s:%d", host, port))
		}
		args := s.command()
		if first {
			s.log("SRT out: " + cmdline(args))
			first = false
		}
		p, err := startFFmpeg(args, true, "SRT out", s.log, &s.seen)
		if err != nil {
			s.set("error", "can't start ffmpeg: "+err.Error())
			s.log("SRT out: can't start ffmpeg: " + err.Error())
			return
		}
		s.mu.Lock()
		s.proc = p
		s.mu.Unlock()
		if s.ctl.stopped() {
			p.stop()
			break
		}
		go s.progress(p, flowing)
		if s.waitUntilOpen(p, flowing) {
			s.clearQueue()
			s.pump(p)
		}
		wasFlowing := isClosed(flowing)
		p.stop()
		if s.ctl.stopped() {
			break
		}
		if wasFlowing {
			backoff = time.Second
			s.log("SRT out: connection closed, restarting")
		}
		s.set("reconnecting", fmt.Sprintf("retry in %.0f s", backoff.Seconds()))
		s.ctl.sleep(backoff)
		if mode == "listener" {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, 8*time.Second)
		}
	}
	s.set("stopped", "")
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
