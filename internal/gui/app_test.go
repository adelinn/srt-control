package gui

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/adelinn/srt-control/internal/bridge"
)

func newTestUI(t *testing.T, cfg bridge.Config) *ui {
	a := test.NewTempApp(t)
	u := &ui{app: a, cfg: cfg, quit: make(chan struct{})}
	u.win = a.NewWindow("test")
	u.win.SetContent(u.build())
	u.fill()
	return u
}

// Settings must survive the trip into the widgets and back unchanged.
func TestSettingsRoundTrip(t *testing.T) {
	cfg := bridge.DefaultConfig()
	cfg.MonitorSource = bridge.FromDevice
	cfg.SendSource = bridge.FromSRT
	cfg.SrtIn = bridge.SrtEndpoint{Mode: "caller", Host: "10.0.0.2", Port: 9100, LatencyMs: 300, StreamID: "in"}
	cfg.SrtOut = bridge.SrtEndpoint{Mode: "listener", Port: 9200, LatencyMs: 80, Passphrase: "0123456789abc"}
	cfg.LowLatencyInput = false
	cfg.MonitorVolume = 0.42
	cfg.MonitorMuted = true
	cfg.MonitorBufferMs = 150
	cfg.Codec = "Opus"
	cfg.BitrateK = 96
	cfg.KeepaliveSilence = false
	cfg.SavePassphrases = true
	cfg.FFmpeg = `C:\tools\ffmpeg.exe`
	cfg.RawLowLatency = true
	cfg.SrtOut.UseCustomURL = true
	cfg.SrtOut.CustomURL = "srt://10.0.0.9:9300?mode=caller&latency=20000&rcvbuf=1000000"

	u := newTestUI(t, cfg)
	got, err := u.collect()
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg {
		t.Errorf("round trip changed settings\n got %+v\nwant %+v", got, cfg)
	}
}

func TestEnabledFollowsChoices(t *testing.T) {
	u := newTestUI(t, bridge.DefaultConfig())
	if u.in.host.Disabled() || u.inDevice.Disabled() || u.out.port.Disabled() {
		t.Error("both inputs and the SRT output should be editable")
	}
	if routeKey(u.monitorFrom) != bridge.FromSRT || routeKey(u.sendFrom) != bridge.FromDevice {
		t.Error("defaults should hear the SRT input and send the local input")
	}
	u.codec.SetSelected("PCM (SMPTE 302M)")
	if !u.bitrate.Disabled() {
		t.Error("bitrate should be disabled for PCM")
	}
	u.send.SetChecked(false)
	if !u.out.port.Disabled() || !u.codec.Disabled() || !u.sendFrom.Disabled() {
		t.Error("output fields should be disabled when not sending")
	}
	if u.in.host.Disabled() || u.monitorFrom.Disabled() {
		t.Error("the SRT input and monitor stay usable without sending")
	}
}

func TestOverallBadge(t *testing.T) {
	c := func(s string) bridge.ComponentStatus { return bridge.ComponentStatus{State: s} }
	for _, tc := range []struct {
		running bool
		st      bridge.Status
		want    string
	}{
		{false, bridge.Status{}, "STOPPED"},
		{true, bridge.Status{SrtIn: c("receiving"), Input: c("capturing"), Monitor: c("buffering"), Sender: c("sending")}, "LIVE"},
		{true, bridge.Status{SrtIn: c("receiving"), Input: c("off"), Monitor: c("playing"), Sender: c("off")}, "LIVE"},
		{true, bridge.Status{SrtIn: c("receiving"), Input: c("capturing"), Monitor: c("playing"), Sender: c("connecting")}, "CONNECTING"},
		{true, bridge.Status{SrtIn: c("no audio"), Input: c("capturing"), Monitor: c("playing"), Sender: c("sending")}, "NO AUDIO"},
		{true, bridge.Status{SrtIn: c("receiving"), Input: c("capturing"), Monitor: c("error"), Sender: c("sending")}, "ERROR"},
	} {
		if got, _ := overall(tc.running, tc.st); got != tc.want {
			t.Errorf("%+v: got %s, want %s", tc.st, got, tc.want)
		}
	}
}

func TestCollectReportsBadInput(t *testing.T) {
	u := newTestUI(t, bridge.DefaultConfig())
	u.out.port.SetText("90x1")
	if _, err := u.collect(); err == nil || !strings.Contains(err.Error(), "SRT out: port must be a number") {
		t.Errorf("got %v", err)
	}
	if !strings.HasPrefix(u.out.url.Text, "Not valid yet") {
		t.Errorf("URL preview should flag the error, shows %q", u.out.url.Text)
	}
	u.out.port.SetText("9001")
	u.buffer.SetText("5")
	if _, err := u.collect(); err == nil || !strings.Contains(err.Error(), "20-1000") {
		t.Errorf("got %v", err)
	}
}

func TestEditURL(t *testing.T) {
	u := newTestUI(t, bridge.DefaultConfig())
	u.in.edit.SetChecked(true) // starts from the URL the fields give
	if !strings.HasPrefix(u.in.custom.Text, "srt://0.0.0.0:9000?mode=listener&transtype=live&latency=120000") {
		t.Errorf("prefilled %q", u.in.custom.Text)
	}
	if !u.in.port.Disabled() || u.in.custom.Hidden || !u.in.url.Hidden {
		t.Error("while editing the URL, the fields should be greyed out and the editor shown")
	}
	u.in.custom.SetText("srt://:9000?mode=pull")
	if !strings.Contains(u.in.hint.seg.Text, "unknown mode") {
		t.Errorf("hint %q", u.in.hint.seg.Text)
	}
	u.in.custom.SetText("srt://:9050?mode=listener&latency=20000")
	cfg, err := u.collect()
	if err != nil || !cfg.SrtIn.UseCustomURL || cfg.SrtIn.CustomURL != "srt://:9050?mode=listener&latency=20000" {
		t.Errorf("collect: %v %+v", err, cfg.SrtIn)
	}
	u.in.edit.SetChecked(false)
	if u.in.port.Disabled() || !u.in.custom.Hidden {
		t.Error("turning Edit URL off should bring the fields back")
	}
	u.rawLatency.SetChecked(true)
	if !u.lowLatency.Disabled() {
		t.Error("raw low latency should grey out low-latency start")
	}
}
