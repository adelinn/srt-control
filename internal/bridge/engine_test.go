package bridge

import (
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
)

// A burst of old audio (decoder probe, reconnect) must be cut to MAX_AHEAD
// instead of becoming permanent latency.
func TestGovernorDropsBursts(t *testing.T) {
	e := NewEngine(DefaultConfig(), func(string) {})
	block := make([]byte, BlockBytes)
	binary.LittleEndian.PutUint16(block[0:], uint16(16384))  // left peak 0.5
	binary.LittleEndian.PutUint16(block[2:], uint16(0xC000)) // right -16384
	for i := 0; i < 100; i++ {                               // 1 s of audio, all at once
		e.onPCM(inSRT, block)
	}
	path := &e.paths[inSRT]
	passed := float64(path.clockFrames) / Rate
	if passed < 0.19 || passed > 0.21 {
		t.Errorf("passed %.3f s of a 1 s burst, want about %.2f s", passed, maxAhead)
	}
	if path.dropped != 100*BlockFrames-path.clockFrames {
		t.Errorf("dropped %d frames", path.dropped)
	}
	if e.paths[inDevice].clockFrames != 0 {
		t.Error("the other input's governor moved")
	}
	p := e.TakePeaks()
	if p.SrtIn[0] != 0.5 || p.SrtIn[1] != 0.5 || p.Input[0] != 0 {
		t.Errorf("peaks %+v", p)
	}
	if p = e.TakePeaks(); p.SrtIn[0] != 0 {
		t.Errorf("peaks not reset: %+v", p)
	}
}

func TestSenderQueueDropsOldest(t *testing.T) {
	s := newSender(DefaultConfig(), "ffmpeg", func(string) {})
	for i := 0; i < 25; i++ {
		s.push([]byte{byte(i)})
	}
	if len(s.q) != cap(s.q) {
		t.Fatalf("queue holds %d", len(s.q))
	}
	if first := <-s.q; first[0] != 5 {
		t.Errorf("oldest kept block is %d, want 5", first[0])
	}
}

func TestEngineRejectsBadSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh as a stand-in ffmpeg path")
	}
	cfg := DefaultConfig()
	cfg.FFmpeg = "/bin/sh" // any existing file: validation runs before ffmpeg is used
	cfg.MonitorEnabled = false
	cfg.SendSource = FromSRT // relay, so SRT in is used
	cfg.SrtOut.Mode, cfg.SrtOut.Port = "listener", cfg.SrtIn.Port
	if err := NewEngine(cfg, func(string) {}).Start(); err == nil {
		t.Error("two listeners on one port accepted")
	}
	cfg = DefaultConfig()
	cfg.MonitorEnabled, cfg.SendEnabled = false, false
	if err := NewEngine(cfg, func(string) {}).Start(); err == nil || !strings.Contains(err.Error(), "nothing to do") {
		t.Errorf("no outputs: got %v", err)
	}
	cfg = DefaultConfig()
	cfg.FFmpeg = "/definitely/not/here/ffmpeg"
	if err := NewEngine(cfg, func(string) {}).Start(); err == nil {
		t.Error("missing ffmpeg accepted")
	}
}
