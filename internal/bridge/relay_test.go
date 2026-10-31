package bridge

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// End-to-end check: tone pulses over SRT -> engine (monitoring off) -> SRT -> receiver.
// Needs ffmpeg with SRT, no sound card. Passes when every pulse arrives intact
// (no dropouts) within -relay.max-latency.
//
//	go test ./internal/bridge -run Relay -v
//	go test ./internal/bridge -run Relay -v -args -relay.codec=Opus
//	go test ./internal/bridge -run Relay/calls -v -args -relay.latency=20 -relay.raw -relay.fast-harness

var (
	relayCodec      = flag.String("relay.codec", "AAC", "output codec: AAC, MP2, Opus or 'PCM (SMPTE 302M)'")
	relaySeconds    = flag.Float64("relay.seconds", 10, "how long to listen")
	relayMaxLatency = flag.Float64("relay.max-latency", 0.8, "seconds")
	relayLatency    = flag.Int("relay.latency", 120, "SRT latency in ms on every hop")
	relayRaw        = flag.Bool("relay.raw", false, "raw low latency input (probesize 32)")
	relayFast       = flag.Bool("relay.fast-harness", false, "make the test's own encoder and receiver low-latency too")
)

const (
	pulsePeriod = 2.0  // seconds between pulse starts
	pulseLen    = 0.3  // pulse length in seconds
	loudRMS     = 3000 // RMS threshold for "tone present"
)

func TestRelay(t *testing.T) {
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
	cases := []struct {
		name      string
		outMode   string
		joinAfter time.Duration
		port      int
		customURL bool
	}{
		{"bridge calls the destination", "caller", 0, 9700, false},
		{"destination joins late", "listener", 5 * time.Second, 9710, false},
		{"hand-edited URLs", "caller", 0, 9720, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runRelay(t, ffmpeg, c.outMode, c.joinAfter, c.port, c.customURL) })
	}
}

type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuffer) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func runRelay(t *testing.T, ffmpeg, outMode string, joinAfter time.Duration, inPort int, customURL bool) {
	outPort := inPort + 1
	rcvMode := "listener"
	if outMode == "listener" {
		rcvMode = "caller"
	}
	cfg := DefaultConfig()
	cfg.FFmpeg = ffmpeg
	cfg.SendSource = FromSRT // relay
	cfg.SrtIn = SrtEndpoint{Mode: "listener", Port: inPort, LatencyMs: *relayLatency}
	cfg.RawLowLatency = *relayRaw
	cfg.MonitorEnabled = false
	cfg.SendEnabled = true
	cfg.SrtOut = SrtEndpoint{Mode: outMode, Host: "127.0.0.1", Port: outPort, LatencyMs: *relayLatency}
	cfg.Codec = *relayCodec
	cfg.BitrateK = 192
	if customURL { // Advanced: URLs typed by hand, fields left at nonsense values
		cfg.SrtIn = SrtEndpoint{Mode: "caller", Port: 1, UseCustomURL: true,
			CustomURL: fmt.Sprintf("srt://:%d?mode=listener&transtype=live&latency=%d&pkt_size=1316", inPort, *relayLatency*1000)}
		cfg.SrtOut = SrtEndpoint{Mode: "listener", Port: 1, UseCustomURL: true,
			CustomURL: fmt.Sprintf("srt://127.0.0.1:%d?latency=%d&pkt_size=1316", outPort, *relayLatency*1000)}
	}

	var procs []*exec.Cmd
	defer func() {
		for _, p := range procs {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}()
	startReceiver := func() io.Reader {
		host := "127.0.0.1"
		if rcvMode == "listener" {
			host = "0.0.0.0"
		}
		probe := []string{"-probesize", "32768", "-analyzeduration", "250000"}
		if *relayFast {
			probe = []string{"-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0"}
		}
		args := append([]string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer"}, probe...)
		args = append(args, "-i", fmt.Sprintf("srt://%s:%d?mode=%s&latency=%d", host, outPort, rcvMode, *relayLatency*1000),
			"-f", "s16le", "-ac", "1", "-ar", fmt.Sprint(Rate), "pipe:1")
		cmd := exec.Command(ffmpeg, args...)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
		return out
	}

	var receiver io.Reader
	if rcvMode == "listener" {
		receiver = startReceiver()
	}
	logs := &logBuffer{}
	engine := NewEngine(cfg, logs.add)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	time.Sleep(1500 * time.Millisecond)

	encArgs := []string{"-hide_banner", "-loglevel", "error", "-probesize", "32", "-analyzeduration", "0",
		"-f", "s16le", "-ar", fmt.Sprint(Rate), "-ac", "2", "-i", "pipe:0",
		"-c:a", "aac", "-b:a", "160k", "-f", "mpegts", "-flush_packets", "1", "-muxdelay", "0"}
	if *relayFast {
		encArgs = append(encArgs, "-muxpreload", "0", "-pes_payload_size", "0")
	}
	encoder := exec.Command(ffmpeg, append(encArgs,
		fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=%d&pkt_size=1316", inPort, *relayLatency*1000))...)
	encIn, err := encoder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Start(); err != nil {
		t.Fatal(err)
	}
	procs = append(procs, encoder)
	t0 := time.Now()
	go feedPulses(encIn, t0)

	if receiver == nil {
		time.Sleep(joinAfter)
		receiver = startReceiver()
	}
	joined := time.Since(t0).Seconds()

	// Stop reading at the deadline by killing the receiver.
	deadline := time.Duration((joined + *relaySeconds) * float64(time.Second))
	rcv := procs[len(procs)-1]
	if rcvMode == "listener" {
		rcv = procs[0]
	}
	timer := time.AfterFunc(deadline-time.Since(t0), func() { _ = rcv.Process.Kill() })
	defer timer.Stop()

	type onset struct{ at, length float64 }
	var onsets []onset
	buf := make([]byte, BlockFrames*2)
	start, length := -1.0, 0.0
	for {
		if _, err := io.ReadFull(receiver, buf); err != nil {
			break
		}
		now := time.Since(t0).Seconds()
		var sum float64
		for i := 0; i < len(buf); i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(buf[i:])))
			sum += v * v
		}
		loud := math.Sqrt(sum/float64(BlockFrames)) > loudRMS
		if loud && start < 0 {
			start, length = now, 0
		}
		if start >= 0 {
			if loud {
				length += float64(BlockFrames) / Rate
			} else {
				onsets = append(onsets, onset{start, length})
				start = -1
			}
		}
	}

	// Skip pulses in the receiver's own start-up burst (its probe makes them arrive bunched up).
	var lat, lens []string
	var lats []float64
	settled := 0
	var problems []string
	for _, o := range onsets {
		if o.at <= joined+2.0 {
			continue
		}
		settled++
		l := math.Mod(o.at, pulsePeriod)
		lat = append(lat, fmt.Sprintf("%.0f ms", l*1000))
		lats = append(lats, l)
		lens = append(lens, fmt.Sprintf("%.0f ms", o.length*1000))
		if l > *relayMaxLatency {
			problems = append(problems, fmt.Sprintf("latency %.0f ms above %.0f ms", l*1000, *relayMaxLatency*1000))
		}
		if math.Abs(o.length-pulseLen) > 0.04 {
			problems = append(problems, fmt.Sprintf("pulse of %.0f ms: cut short or split (dropout)", o.length*1000))
		}
	}
	expected := int(*relaySeconds-2.0-*relayMaxLatency) / int(pulsePeriod)
	t.Logf("SRT out %s, codec %s, receiver joined at %.1f s", outMode, cfg.Codec, joined)
	t.Logf("latency per pulse: %s", strings.Join(lat, " "))
	if len(lats) > 0 {
		sort.Float64s(lats)
		t.Logf("median latency:    %.0f ms (SRT latency %d ms per hop, raw input %v, fast harness %v)",
			lats[len(lats)/2]*1000, *relayLatency, *relayRaw, *relayFast)
	}
	t.Logf("pulse length:      %s", strings.Join(lens, " "))
	if settled < max(1, expected) {
		problems = append(problems, fmt.Sprintf("only %d pulses arrived, expected at least %d", settled, expected))
	}
	if len(problems) > 0 {
		t.Errorf("%s\nengine log:\n%s", strings.Join(problems, "\n"), logs)
	}
}

// feedPulses writes 1 kHz pulses paced to the wall clock, like a live source.
func feedPulses(w io.WriteCloser, t0 time.Time) {
	defer w.Close()
	buf := make([]byte, BlockBytes)
	for n := 0; ; n += BlockFrames {
		for i := 0; i < BlockFrames; i++ {
			ts := float64(n+i) / Rate
			v := 0.0
			if math.Mod(ts, pulsePeriod) < pulseLen {
				v = 0.5 * math.Sin(2*math.Pi*1000*ts)
			}
			s := uint16(int16(v * 32767))
			binary.LittleEndian.PutUint16(buf[i*4:], s)
			binary.LittleEndian.PutUint16(buf[i*4+2:], s)
		}
		if _, err := w.Write(buf); err != nil {
			return
		}
		if d := time.Until(t0.Add(time.Duration(float64(n+BlockFrames) / Rate * float64(time.Second)))); d > 0 {
			time.Sleep(d)
		}
	}
}
