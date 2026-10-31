package bridge

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// The required setup: hear the SRT input on the monitor while the local input
// (a mic) goes out over SRT, both at once. The mic and the monitor are fakes,
// because CI has no sound card; ffmpeg, SRT and the engine are real.
//
//	go test ./internal/bridge -run Talkback -v

// fakeMic plays a sine tone in real time, like a sound card would deliver it.
type fakeMic struct {
	statusBox
	onPCM func([]byte)
	freq  float64
	ctl   stopper
}

func (m *fakeMic) start() error {
	m.set("capturing", "fake mic")
	go func() {
		defer close(m.ctl.done)
		t0 := time.Now()
		for n := 0; !m.ctl.stopped(); n += BlockFrames {
			buf := make([]byte, BlockBytes) // handed on, so never reused
			for i := 0; i < BlockFrames; i++ {
				s := uint16(int16(0.3 * 32767 * math.Sin(2*math.Pi*m.freq*float64(n+i)/Rate)))
				binary.LittleEndian.PutUint16(buf[i*4:], s)
				binary.LittleEndian.PutUint16(buf[i*4+2:], s)
			}
			m.onPCM(buf)
			m.ctl.sleep(time.Until(t0.Add(time.Duration(n+BlockFrames) * time.Second / Rate)))
		}
	}()
	return nil
}

func (m *fakeMic) stop() {
	m.ctl.signal()
	<-m.ctl.done
	m.set("stopped", "")
}

func (m *fakeMic) status() (string, string) { return m.get() }

// fakeMonitor keeps everything it is asked to play.
type fakeMonitor struct {
	mu  sync.Mutex
	pcm []byte
}

func (f *fakeMonitor) push(data []byte) {
	f.mu.Lock()
	f.pcm = append(f.pcm, data...)
	f.mu.Unlock()
}
func (f *fakeMonitor) setVolume(float64, bool)  {}
func (f *fakeMonitor) stop()                    {}
func (f *fakeMonitor) status() (string, string) { return "playing", "fake monitor" }

func (f *fakeMonitor) tail(seconds float64) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return tailOf(f.pcm, int(seconds*Rate)*FrameBytes)
}

func tailOf(b []byte, n int) []byte {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return append([]byte(nil), b...)
}

// toneOf estimates the main frequency (from zero crossings) and RMS level of
// s16le audio, using the first channel.
func toneOf(pcm []byte, channels int) (hz, dbfs float64) {
	step := 2 * channels
	n := len(pcm) / step
	if n < Rate/10 {
		return 0, -120
	}
	var sum float64
	crossings := 0
	prev := int16(binary.LittleEndian.Uint16(pcm))
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(pcm[i*step:]))
		sum += float64(v) * float64(v)
		if (v < 0) != (prev < 0) {
			crossings++
		}
		prev = v
	}
	rms := math.Sqrt(sum / float64(n))
	return float64(crossings) / 2 / (float64(n) / Rate), 20 * math.Log10(max(rms, 1e-3)/32768)
}

func TestTalkback(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	ffmpeg := FindFFmpeg("ffmpeg")
	if ffmpeg == "" {
		t.Skip("ffmpeg not found")
	}
	if info, err := ProbeFFmpeg(ffmpeg); err != nil || !info.SRT {
		t.Skip("ffmpeg has no SRT support")
	}

	// Swap the sound-card parts for fakes.
	savedIn, savedMon := openInputDevice, openMonitor
	t.Cleanup(func() { openInputDevice, openMonitor = savedIn, savedMon })
	openInputDevice = func(_, _ string, onPCM func([]byte), _ func(string)) (source, error) {
		return &fakeMic{onPCM: onPCM, freq: 1000, ctl: newStopper()}, nil
	}
	var monMu sync.Mutex
	var monitors []*fakeMonitor
	openMonitor = func(string, string, int, float64, bool, func(string)) (monitorOut, error) {
		m := &fakeMonitor{}
		monMu.Lock()
		monitors = append(monitors, m)
		monMu.Unlock()
		return m, nil
	}
	currentMonitor := func() *fakeMonitor {
		monMu.Lock()
		defer monMu.Unlock()
		return monitors[len(monitors)-1]
	}

	const inPort, outPort = 9730, 9731
	cfg := DefaultConfig() // listen to SRT in, send the local input
	cfg.FFmpeg = ffmpeg
	cfg.SrtIn = SrtEndpoint{Mode: "listener", Port: inPort, LatencyMs: 120}
	cfg.SrtOut = SrtEndpoint{Mode: "caller", Host: "127.0.0.1", Port: outPort, LatencyMs: 120}
	if cfg.MonitorSource != FromSRT || cfg.SendSource != FromDevice || !cfg.MonitorEnabled || !cfg.SendEnabled {
		t.Fatalf("defaults aren't the talkback setup: %+v", cfg)
	}

	var procs []*exec.Cmd
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}()
	run := func(stdout io.Writer, args ...string) {
		cmd := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
		cmd.Stdout = stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
	}

	// The far end that receives the mic.
	var rxMu sync.Mutex
	var rx bytes.Buffer
	run(writerFunc(func(p []byte) (int, error) {
		rxMu.Lock()
		defer rxMu.Unlock()
		return rx.Write(p)
	}), "-fflags", "nobuffer", "-i", fmt.Sprintf("srt://0.0.0.0:%d?mode=listener&latency=120000", outPort),
		"-f", "s16le", "-ac", "1", "-ar", fmt.Sprint(Rate), "pipe:1")

	logs := &logBuffer{}
	engine := NewEngine(cfg, logs.add)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()

	// The far end that sends the programme feed we want to hear.
	run(nil, "-re", "-f", "lavfi", "-i", fmt.Sprintf("sine=f=440:r=%d", Rate), "-ac", "2",
		"-c:a", "aac", "-f", "mpegts", fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=120000&pkt_size=1316", inPort))

	time.Sleep(6 * time.Second)
	st := engine.Snapshot()
	if st.SrtIn.State != "receiving" || st.Input.State != "capturing" || st.Sender.State != "sending" {
		t.Errorf("status %+v\nengine log:\n%s", st, logs)
	}
	check := func(what string, pcm []byte, channels int, wantHz float64) {
		hz, db := toneOf(pcm, channels)
		t.Logf("%s: %.0f Hz at %.1f dBFS", what, hz, db)
		if math.Abs(hz-wantHz) > wantHz*0.05 || db < -30 {
			t.Errorf("%s: got %.0f Hz at %.1f dBFS, want %.0f Hz\nengine log:\n%s", what, hz, db, wantHz, logs)
		}
	}
	check("monitor hears (SRT in)", currentMonitor().tail(3), Channels, 440)
	rxMu.Lock()
	sent := tailOf(rx.Bytes(), 3*Rate*2)
	rxMu.Unlock()
	check("far end receives (local input)", sent, 1, 1000)

	// Switch the monitor to the local input while running.
	if err := engine.SetMonitor(true, FromDevice, "", "", 100); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	check("monitor hears after switching (local input)", currentMonitor().tail(1.5), Channels, 1000)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
