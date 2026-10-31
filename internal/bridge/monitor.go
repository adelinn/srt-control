package bridge

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
)

// monitorSink plays the PCM on an output device through a small jitter buffer.
// Excess is dropped so monitoring latency stays near the buffer size.
type monitorSink struct {
	statusBox
	log      func(string)
	dev      *malgo.Device
	bufferMs int
	stopping atomic.Bool

	mu        sync.Mutex
	buf       []byte
	target    int // bytes
	limit     int // bytes
	primed    bool
	volume    float32
	muted     bool
	underruns int
	trims     int
}

func newMonitor(backend, name string, bufferMs int, volume float64, muted bool, log func(string)) (*monitorSink, error) {
	m := &monitorSink{log: log, bufferMs: bufferMs, volume: float32(volume), muted: muted}
	m.target = max(1, Rate*bufferMs/1000) * FrameBytes
	m.limit = m.target*2 + Rate/20*FrameBytes // target x2 + 50 ms
	m.buf = make([]byte, 0, m.limit+8*BlockBytes)
	dev, label, err := openDevice(backend, name, false, malgo.DeviceCallbacks{Data: m.callback, Stop: m.onStop})
	if err != nil {
		return nil, err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, fmt.Errorf("can't start output device: %w", err)
	}
	m.dev = dev
	m.set("playing", label)
	log("Monitor: " + label)
	return m, nil
}

func (m *monitorSink) push(data []byte) {
	m.mu.Lock()
	m.buf = append(m.buf, data...)
	if len(m.buf) > m.limit {
		n := copy(m.buf, m.buf[len(m.buf)-m.target:])
		m.buf = m.buf[:n]
		m.trims++
	}
	m.mu.Unlock()
}

func (m *monitorSink) callback(out, _ []byte, _ uint32) {
	need := len(out)
	have := false
	m.mu.Lock()
	if !m.primed && len(m.buf) >= m.target {
		m.primed = true
	}
	if m.primed && len(m.buf) >= need {
		copy(out, m.buf[:need])
		m.buf = m.buf[:copy(m.buf, m.buf[need:])]
		have = true
	} else if m.primed {
		m.primed = false // underrun: refill to target before playing again
		m.underruns++
	}
	vol, muted := m.volume, m.muted
	m.mu.Unlock()

	if !have || muted {
		clear(out)
		return
	}
	if vol != 1 {
		for i := 0; i+1 < len(out); i += 2 {
			v := float32(int16(binary.LittleEndian.Uint16(out[i:]))) * vol
			v = float32(math.Max(-32768, math.Min(32767, float64(v))))
			binary.LittleEndian.PutUint16(out[i:], uint16(int16(v)))
		}
	}
}

func (m *monitorSink) onStop() {
	if !m.stopping.Load() {
		m.set("error", "output device stopped")
		m.log("Monitor: output device stopped (unplugged?)")
	}
}

func (m *monitorSink) setVolume(volume float64, muted bool) {
	m.mu.Lock()
	m.volume, m.muted = float32(volume), muted
	m.mu.Unlock()
}

func (m *monitorSink) stop() {
	m.stopping.Store(true)
	m.dev.Uninit()
	m.set("stopped", "")
}

func (m *monitorSink) status() (string, string) {
	state, detail := m.get()
	if state != "playing" {
		return state, detail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.primed {
		state = "buffering"
	}
	return state, fmt.Sprintf("%d ms buffer · %d underruns · %d trims", m.bufferMs, m.underruns, m.trims)
}
