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

// Status of the two inputs and two outputs. State "off" means not running.
type Status struct{ SrtIn, Input, Monitor, Sender ComponentStatus }

// Peaks per input, 0..1 per channel.
type Peaks struct{ SrtIn, Input [Channels]float64 }

type input int

const (
	inSRT input = iota
	inDevice
	numInputs
)

func parseInput(s string) input {
	if s == FromDevice {
		return inDevice
	}
	return inSRT
}

// monitorOut is the monitor as the engine sees it (a fake in tests).
type monitorOut interface {
	push([]byte)
	setVolume(volume float64, muted bool)
	stop()
	status() (string, string)
}

// Constructors for the parts that need a sound card; tests replace them.
var (
	openInputDevice = func(backend, name string, onPCM func([]byte), log func(string)) (source, error) {
		d, err := newDeviceSource(backend, name, onPCM, log)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	openMonitor = func(backend, name string, bufferMs int, volume float64, muted bool, log func(string)) (monitorOut, error) {
		m, err := newMonitor(backend, name, bufferMs, volume, muted, log)
		if err != nil {
			return nil, err
		}
		return m, nil
	}
)

// inputPath is one input with its real-time governor and peak meter.
type inputPath struct {
	name string
	src  source // nil while not running; guarded by Engine.mu

	// governor state, only touched by this input's own thread
	clockRef        time.Time
	clockFrames     int
	dropped         int
	droppedLoggedAt time.Time

	peakMu sync.Mutex
	peak   [Channels]float64
}

type monitorRef struct{ out monitorOut }

// Engine runs up to two inputs (SRT in, local input device) at the same time
// and routes each output to one of them: the monitor plays one, SRT out sends
// one. It knows nothing about the GUI, so headless mode uses it as is.
type Engine struct {
	cfg     Config
	log     func(string)
	running atomic.Bool
	ffmpeg  string

	mu          sync.Mutex // serializes Start, Stop and monitor changes
	paths       [numInputs]inputPath
	monitor     atomic.Pointer[monitorRef]
	monitorFrom atomic.Int32 // input the monitor plays
	sender      atomic.Pointer[srtSender]
	sendFrom    input // input SRT out sends; fixed while running
}

// NewEngine prepares an engine. log must be safe to call from any goroutine;
// passphrases are masked before it sees a message.
func NewEngine(cfg Config, log func(string)) *Engine {
	cfg.normalize()
	e := &Engine{cfg: cfg, log: func(msg string) { log(MaskSecrets(msg)) }}
	e.paths[inSRT].name = "SRT in"
	e.paths[inDevice].name = "Local input"
	return e
}

// Running reports whether Start succeeded and Stop hasn't been called.
func (e *Engine) Running() bool { return e.running.Load() }

// Start validates the settings, then starts the outputs that are on and the
// inputs they use.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := e.cfg
	monFrom, sendFrom := parseInput(cfg.MonitorSource), parseInput(cfg.SendSource)
	uses := func(in input) bool {
		return (cfg.MonitorEnabled && monFrom == in) || (cfg.SendEnabled && sendFrom == in)
	}
	if !cfg.MonitorEnabled && !cfg.SendEnabled {
		return errors.New("nothing to do: turn on Monitor or Send over SRT")
	}
	if uses(inSRT) {
		if err := e.checkSrtIn(); err != nil {
			return err
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
	if uses(inSRT) || cfg.SendEnabled {
		var err error
		if e.ffmpeg, err = ResolveFFmpeg(cfg.FFmpeg, e.log); err != nil {
			return err
		}
	}

	e.sendFrom = sendFrom
	e.monitorFrom.Store(int32(monFrom))
	fail := func(err error) error {
		e.stopLocked()
		return err
	}
	if cfg.MonitorEnabled {
		m, err := openMonitor(cfg.HostAPI, cfg.MonitorDevice, cfg.MonitorBufferMs, cfg.MonitorVolume, cfg.MonitorMuted, e.log)
		if err != nil {
			return fail(fmt.Errorf("monitor: %w", err))
		}
		e.monitor.Store(&monitorRef{m})
	}
	if cfg.SendEnabled {
		s := newSender(cfg, e.ffmpeg, e.log)
		e.sender.Store(s)
		s.start()
	}
	for in := input(0); in < numInputs; in++ {
		if uses(in) {
			if err := e.startInput(in); err != nil {
				return fail(err)
			}
		}
	}
	e.running.Store(true)
	return nil
}

func (e *Engine) checkSrtIn() error {
	if err := e.cfg.SrtIn.Validate(); err != nil {
		return fmt.Errorf("SRT in: %w", err)
	}
	if e.cfg.SendEnabled && e.cfg.SrtIn.Mode == "listener" && e.cfg.SrtOut.Mode == "listener" &&
		e.cfg.SrtIn.Port == e.cfg.SrtOut.Port {
		return errors.New("SRT in and SRT out can't both listen on the same port")
	}
	return nil
}

// startInput starts an input unless it is already running. Caller holds mu.
func (e *Engine) startInput(in input) error {
	p := &e.paths[in]
	if p.src != nil {
		return nil
	}
	onPCM := func(data []byte) { e.onPCM(in, data) }
	var src source
	switch in {
	case inSRT:
		if err := e.checkSrtIn(); err != nil {
			return err
		}
		if e.ffmpeg == "" {
			f, err := ResolveFFmpeg(e.cfg.FFmpeg, e.log)
			if err != nil {
				return err
			}
			e.ffmpeg = f
		}
		src = newSrtSource(e.cfg, e.ffmpeg, onPCM, e.log)
	case inDevice:
		d, err := openInputDevice(e.cfg.HostAPI, e.cfg.InputDevice, onPCM, e.log)
		if err != nil {
			return fmt.Errorf("local input: %w", err)
		}
		src = d
	}
	if err := src.start(); err != nil {
		src.stop()
		return err
	}
	p.src = src
	return nil
}

// Stop stops everything. It is safe to call more than once.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *Engine) stopLocked() {
	e.running.Store(false)
	for in := range e.paths {
		if src := e.paths[in].src; src != nil {
			src.stop()
			e.paths[in].src = nil
		}
	}
	if s := e.sender.Swap(nil); s != nil {
		s.stop()
	}
	if m := e.monitor.Swap(nil); m != nil {
		m.out.stop()
	}
}

// SetMonitor switches monitoring on or off, to the other input, or to another
// audio API, device or buffer size, while running. An input that isn't
// running yet is started.
func (e *Engine) SetMonitor(enabled bool, from, backend, device string, bufferMs int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if old := e.monitor.Swap(nil); old != nil {
		old.out.stop()
	}
	in := parseInput(from)
	e.monitorFrom.Store(int32(in))
	e.cfg.MonitorEnabled, e.cfg.MonitorSource = enabled, from
	e.cfg.MonitorDevice, e.cfg.MonitorBufferMs = device, bufferMs
	if !enabled || !e.running.Load() {
		return nil
	}
	if err := e.startInput(in); err != nil {
		return err
	}
	m, err := openMonitor(backend, device, bufferMs, e.cfg.MonitorVolume, e.cfg.MonitorMuted, e.log)
	if err != nil {
		return err
	}
	e.monitor.Store(&monitorRef{m})
	return nil
}

// SetVolume changes monitor volume and mute while running.
func (e *Engine) SetVolume(volume float64, muted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.MonitorVolume, e.cfg.MonitorMuted = volume, muted
	if m := e.monitor.Load(); m != nil {
		m.out.setVolume(volume, muted)
	}
}

// onPCM handles every block of audio from one input.
func (e *Engine) onPCM(in input, data []byte) {
	p := &e.paths[in]
	frames := len(data) / FrameBytes
	now := time.Now()
	// Real-time governor: after a probe or a reconnect, ffmpeg can hand over a
	// burst of old audio at once (and an unclocked device can flood). Passing
	// it on would add that much latency to everything downstream, so drop
	// what's too early.
	ahead := func(f int) float64 { return float64(f)/Rate - now.Sub(p.clockRef).Seconds() }
	if p.clockRef.IsZero() || ahead(p.clockFrames) < -0.5 {
		p.clockRef, p.clockFrames = now, 0
	}
	if ahead(p.clockFrames+frames) > maxAhead {
		p.dropped += frames
		return
	}
	if p.dropped > 0 && now.Sub(p.droppedLoggedAt) > 10*time.Second { // report at most every 10 s
		if ms := p.dropped * 1000 / Rate; ms >= 20 {
			e.log(fmt.Sprintf("%s: dropped %d ms of audio that arrived faster than real time (keeps latency low)", p.name, ms))
		}
		p.dropped, p.droppedLoggedAt = 0, now
	}
	p.clockFrames += frames

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
	p.peakMu.Lock()
	for ch := range peak {
		p.peak[ch] = max(p.peak[ch], float64(peak[ch])/32768)
	}
	p.peakMu.Unlock()

	if input(e.monitorFrom.Load()) == in {
		if m := e.monitor.Load(); m != nil {
			m.out.push(data)
		}
	}
	if e.sendFrom == in {
		if s := e.sender.Load(); s != nil {
			s.push(data)
		}
	}
}

// TakePeaks returns each input's per-channel peak (0..1) since the last call.
func (e *Engine) TakePeaks() Peaks {
	take := func(p *inputPath) [Channels]float64 {
		p.peakMu.Lock()
		defer p.peakMu.Unlock()
		v := p.peak
		p.peak = [Channels]float64{}
		return v
	}
	return Peaks{SrtIn: take(&e.paths[inSRT]), Input: take(&e.paths[inDevice])}
}

// Snapshot returns the status of both inputs and both outputs.
func (e *Engine) Snapshot() Status {
	get := func(c interface{ status() (string, string) }) ComponentStatus {
		state, detail := c.status()
		return ComponentStatus{state, detail}
	}
	off := ComponentStatus{State: "off"}
	st := Status{SrtIn: off, Input: off, Monitor: off, Sender: off}
	e.mu.Lock()
	if src := e.paths[inSRT].src; src != nil {
		st.SrtIn = get(src)
	}
	if src := e.paths[inDevice].src; src != nil {
		st.Input = get(src)
	}
	e.mu.Unlock()
	if m := e.monitor.Load(); m != nil {
		st.Monitor = get(m.out)
	}
	if s := e.sender.Load(); s != nil {
		st.Sender = get(s)
	}
	return st
}
