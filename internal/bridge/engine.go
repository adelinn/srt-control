package bridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ComponentStatus is what a status light shows.
type ComponentStatus struct{ State, Detail string }

// Status of the three parts of the engine. State "off" means not in use.
type Status struct{ Source, Monitor, Sender ComponentStatus }

// Engine wires a source to the monitor and the SRT sender. It knows nothing
// about the GUI, so headless mode uses it as is.
type Engine struct {
	cfg     Config
	log     func(string)
	running atomic.Bool

	srcMu   sync.Mutex
	source  source
	monMu   sync.Mutex // serializes monitor changes
	monitor atomic.Pointer[monitorSink]
	sender  atomic.Pointer[srtSender]

	peakMu sync.Mutex
	peak   [Channels]float64

	// real-time governor, only touched by the source's thread
	clockRef        time.Time
	clockFrames     int
	dropped         int
	droppedLoggedAt time.Time
}

// NewEngine prepares an engine. log must be safe to call from any goroutine;
// passphrases are masked before it sees a message.
func NewEngine(cfg Config, log func(string)) *Engine {
	return &Engine{cfg: cfg, log: func(msg string) { log(MaskSecrets(msg)) }}
}

// Running reports whether Start succeeded and Stop hasn't been called.
func (e *Engine) Running() bool { return e.running.Load() }

// Start validates the settings and starts monitor, sender and source.
func (e *Engine) Start() error {
	cfg := e.cfg
	ffmpeg := ""
	if cfg.Source == "srt" || cfg.SendEnabled {
		var err error
		if ffmpeg, err = ResolveFFmpeg(cfg.FFmpeg, e.log); err != nil {
			return err
		}
	}
	if cfg.Source != "srt" && cfg.Source != "device" {
		return fmt.Errorf("unknown source '%s'", cfg.Source)
	}
	if cfg.Source == "srt" {
		if err := cfg.SrtIn.Validate(); err != nil {
			return fmt.Errorf("SRT in: %w", err)
		}
	}
	if cfg.SendEnabled {
		if err := cfg.SrtOut.Validate(); err != nil {
			return fmt.Errorf("SRT out: %w", err)
		}
		if _, ok := codecArgs[cfg.Codec]; !ok {
			return fmt.Errorf("unknown codec %s", cfg.Codec)
		}
	}
	if cfg.Source == "srt" && cfg.SendEnabled && cfg.SrtIn.Mode == "listener" &&
		cfg.SrtOut.Mode == "listener" && cfg.SrtIn.Port == cfg.SrtOut.Port {
		return errors.New("SRT in and SRT out can't both listen on the same port")
	}

	fail := func(err error) error {
		e.Stop()
		return err
	}
	if cfg.MonitorEnabled {
		m, err := newMonitor(cfg.HostAPI, cfg.MonitorDevice, cfg.MonitorBufferMs, cfg.MonitorVolume, cfg.MonitorMuted, e.log)
		if err != nil {
			return fail(fmt.Errorf("monitor: %w", err))
		}
		e.monitor.Store(m)
	}
	if cfg.SendEnabled {
		s := newSender(cfg, ffmpeg, e.log)
		e.sender.Store(s)
		s.start()
	}
	var src source
	if cfg.Source == "srt" {
		src = newSrtSource(cfg, ffmpeg, e.onPCM, e.log)
	} else {
		d, err := newDeviceSource(cfg.HostAPI, cfg.InputDevice, e.onPCM, e.log)
		if err != nil {
			return fail(fmt.Errorf("input: %w", err))
		}
		src = d
	}
	e.srcMu.Lock()
	e.source = src
	e.srcMu.Unlock()
	if err := src.start(); err != nil {
		return fail(err)
	}
	e.running.Store(true)
	return nil
}

// Stop stops everything. It is safe to call more than once.
func (e *Engine) Stop() {
	e.running.Store(false)
	e.srcMu.Lock()
	src := e.source
	e.source = nil
	e.srcMu.Unlock()
	if src != nil {
		src.stop()
	}
	if s := e.sender.Swap(nil); s != nil {
		s.stop()
	}
	e.monMu.Lock()
	if m := e.monitor.Swap(nil); m != nil {
		m.stop()
	}
	e.monMu.Unlock()
}

// SetMonitor switches monitoring on or off, or to another audio API, device or
// buffer size, while running.
func (e *Engine) SetMonitor(enabled bool, backend, device string, bufferMs int) error {
	e.monMu.Lock()
	defer e.monMu.Unlock()
	if old := e.monitor.Swap(nil); old != nil {
		old.stop()
	}
	e.cfg.MonitorEnabled, e.cfg.MonitorDevice, e.cfg.MonitorBufferMs = enabled, device, bufferMs
	if !enabled || !e.running.Load() {
		return nil
	}
	m, err := newMonitor(backend, device, bufferMs, e.cfg.MonitorVolume, e.cfg.MonitorMuted, e.log)
	if err != nil {
		return err
	}
	e.monitor.Store(m)
	return nil
}

// SetVolume changes monitor volume and mute while running.
func (e *Engine) SetVolume(volume float64, muted bool) {
	e.monMu.Lock()
	defer e.monMu.Unlock()
	e.cfg.MonitorVolume, e.cfg.MonitorMuted = volume, muted
	if m := e.monitor.Load(); m != nil {
		m.setVolume(volume, muted)
	}
}

// onPCM is the fan-out point for every block of source audio.
func (e *Engine) onPCM(data []byte) {
	frames := len(data) / FrameBytes
	now := time.Now()
	// Real-time governor: after a probe or a reconnect, ffmpeg can hand over a
	// burst of old audio at once. Passing it on would add that much latency to
	// the monitor and to everything downstream, so drop what's too early.
	ahead := func(f int) float64 { return float64(f)/Rate - now.Sub(e.clockRef).Seconds() }
	if e.clockRef.IsZero() || ahead(e.clockFrames) < -0.5 {
		e.clockRef, e.clockFrames = now, 0
	}
	if ahead(e.clockFrames+frames) > maxAhead {
		e.dropped += frames
		return
	}
	if e.dropped > 0 && now.Sub(e.droppedLoggedAt) > 10*time.Second { // report at most every 10 s
		if ms := e.dropped * 1000 / Rate; ms >= 20 {
			e.log(fmt.Sprintf("Dropped %d ms of audio that arrived faster than real time (keeps latency low)", ms))
		}
		e.dropped, e.droppedLoggedAt = 0, now
	}
	e.clockFrames += frames

	var peak [Channels]int
	for i := 0; i+FrameBytes <= len(data); i += FrameBytes {
		for ch := 0; ch < Channels; ch++ {
			v := int(int16(binary.LittleEndian.Uint16(data[i+2*ch:])))
			if v < 0 {
				v = -v
			}
			peak[ch] = max(peak[ch], v)
		}
	}
	e.peakMu.Lock()
	for ch := range peak {
		e.peak[ch] = max(e.peak[ch], float64(peak[ch])/32768)
	}
	e.peakMu.Unlock()

	if m := e.monitor.Load(); m != nil {
		m.push(data)
	}
	if s := e.sender.Load(); s != nil {
		s.push(data)
	}
}

// TakePeaks returns the per-channel peak (0..1) since the last call.
func (e *Engine) TakePeaks() [Channels]float64 {
	e.peakMu.Lock()
	defer e.peakMu.Unlock()
	p := e.peak
	e.peak = [Channels]float64{}
	return p
}

// Snapshot returns the status of source, monitor and sender.
func (e *Engine) Snapshot() Status {
	get := func(c interface{ status() (string, string) }) ComponentStatus {
		state, detail := c.status()
		return ComponentStatus{state, detail}
	}
	st := Status{Source: ComponentStatus{State: "off"}, Monitor: ComponentStatus{State: "off"}, Sender: ComponentStatus{State: "off"}}
	e.srcMu.Lock()
	if e.source != nil {
		st.Source = get(e.source)
	}
	e.srcMu.Unlock()
	if m := e.monitor.Load(); m != nil {
		st.Monitor = get(m)
	}
	if s := e.sender.Load(); s != nil {
		st.Sender = get(s)
	}
	return st
}
