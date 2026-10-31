package bridge

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"
)

// status is the state shown in the GUI's status lights.
type statusBox struct {
	mu            sync.Mutex
	state, detail string
}

func (s *statusBox) set(state, detail string) {
	s.mu.Lock()
	s.state, s.detail = state, detail
	s.mu.Unlock()
}

func (s *statusBox) get() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.detail
}

// stopper is a close-once stop signal plus a "loop finished" signal.
type stopper struct {
	ch   chan struct{}
	once sync.Once
	done chan struct{}
}

func newStopper() stopper { return stopper{ch: make(chan struct{}), done: make(chan struct{})} }

func (s *stopper) signal() { s.once.Do(func() { close(s.ch) }) }

func (s *stopper) stopped() bool {
	select {
	case <-s.ch:
		return true
	default:
		return false
	}
}

// sleep waits for d or until stopped.
func (s *stopper) sleep(d time.Duration) {
	select {
	case <-s.ch:
	case <-time.After(d):
	}
}

type source interface {
	start() error
	stop()
	status() (string, string)
}

// srtSource runs ffmpeg to read SRT and decode the first audio track to PCM on
// stdout. It restarts on its own when the stream drops or the far end isn't up yet.
type srtSource struct {
	statusBox
	cfg      Config
	ffmpeg   string
	onPCM    func([]byte)
	log      func(string)
	seen     errorDedupe
	ctl      stopper
	mu       sync.Mutex
	proc     *ffproc
	lastData atomic.Int64 // UnixNano of the last block
}

func newSrtSource(cfg Config, ffmpeg string, onPCM func([]byte), log func(string)) *srtSource {
	return &srtSource{cfg: cfg, ffmpeg: ffmpeg, onPCM: onPCM, log: log, ctl: newStopper()}
}

func (s *srtSource) command() []string {
	cmd := []string{s.ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "warning"}
	switch {
	case s.cfg.RawLowLatency:
		// Smallest probe ffmpeg allows: decoding starts with the first packets
		// (verified with AAC, MP2, Opus and SMPTE 302M in MPEG-TS, ffmpeg 6.1).
		cmd = append(cmd, "-fflags", "nobuffer", "-flags", "low_delay",
			"-probesize", "32", "-analyzeduration", "0")
	case s.cfg.LowLatencyInput:
		cmd = append(cmd, "-fflags", "nobuffer", "-flags", "low_delay",
			"-probesize", "32768", "-analyzeduration", "250000")
	}
	url := s.cfg.SrtIn.URL([][2]string{{"timeout", "5000000"}}, false) // µs: give up on a stalled stream
	return append(cmd, "-i", url, "-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", fmt.Sprint(Channels), "-ar", fmt.Sprint(Rate), "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
}

func (s *srtSource) start() error {
	go s.run()
	return nil
}

func (s *srtSource) stop() {
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

func (s *srtSource) status() (string, string) {
	state, detail := s.get()
	if state == "receiving" && time.Since(time.Unix(0, s.lastData.Load())) > 1500*time.Millisecond {
		return "no audio", detail
	}
	return state, detail
}

func (s *srtSource) run() {
	defer close(s.ctl.done)
	mode, host, port, _ := s.cfg.SrtIn.Target() // validated before start
	where := fmt.Sprintf("%s:%d", host, port)
	backoff := time.Second
	first := true
	for !s.ctl.stopped() {
		if mode == "listener" {
			s.set("connecting", "listening on "+where)
		} else {
			s.set("connecting", "connecting to "+where)
		}
		args := s.command()
		if first {
			s.log("SRT in: " + cmdline(args))
			first = false
		}
		p, err := startFFmpeg(args, false, "SRT in", s.log, &s.seen)
		if err != nil {
			s.set("error", "can't start ffmpeg: "+err.Error())
			s.log("SRT in: can't start ffmpeg: " + err.Error())
			return
		}
		s.mu.Lock()
		s.proc = p
		s.mu.Unlock()
		if s.ctl.stopped() { // stop() ran before proc was set
			p.stop()
			break
		}
		receiving := false
		for {
			buf := make([]byte, BlockBytes) // handed on, so never reused
			n, err := io.ReadFull(p.stdout, buf)
			if n -= n % FrameBytes; n > 0 {
				if !receiving {
					receiving, backoff = true, time.Second
					s.set("receiving", where)
					s.log("SRT in: receiving audio")
				}
				s.lastData.Store(time.Now().UnixNano())
				s.onPCM(buf[:n])
			}
			if err != nil {
				break
			}
		}
		p.stop()
		if s.ctl.stopped() {
			break
		}
		if receiving {
			s.log("SRT in: stream ended, reconnecting")
		}
		s.set("reconnecting", fmt.Sprintf("retry in %.0f s", backoff.Seconds()))
		s.ctl.sleep(backoff)
		// a listener should be back on its port quickly; a caller backs off
		if mode == "listener" {
			backoff = time.Second
		} else {
			backoff = min(backoff*2, 8*time.Second)
		}
	}
	s.set("stopped", "")
}

// deviceSource captures a local input device (mic, line-in, virtual cable).
// miniaudio duplicates mono inputs to stereo.
type deviceSource struct {
	statusBox
	onPCM    func([]byte)
	log      func(string)
	dev      *malgo.Device
	label    string
	stopping atomic.Bool
}

func newDeviceSource(backend, name string, onPCM func([]byte), log func(string)) (*deviceSource, error) {
	s := &deviceSource{onPCM: onPCM, log: log}
	dev, label, err := openDevice(backend, name, true, malgo.DeviceCallbacks{Data: s.callback, Stop: s.onStop})
	if err != nil {
		return nil, err
	}
	s.dev, s.label = dev, label
	return s, nil
}

func (s *deviceSource) callback(_, in []byte, _ uint32) {
	if len(in) == 0 {
		return
	}
	s.onPCM(append([]byte(nil), in...)) // miniaudio reuses its buffer
}

func (s *deviceSource) onStop() {
	if !s.stopping.Load() {
		s.set("error", "input device stopped")
		s.log("Input device stopped (unplugged?)")
	}
}

func (s *deviceSource) start() error {
	if err := s.dev.Start(); err != nil {
		return fmt.Errorf("can't start input device: %w", err)
	}
	s.set("capturing", s.label)
	s.log("Input device: " + s.label)
	return nil
}

func (s *deviceSource) stop() {
	s.stopping.Store(true)
	s.dev.Uninit()
	s.set("stopped", "")
}

func (s *deviceSource) status() (string, string) { return s.get() }
