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
	cfg.Source = "device"
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
	if u.in.host.Disabled() || !u.inDevice.Disabled() {
		t.Error("SRT source: SRT fields should be enabled, input device disabled")
	}
	u.source.SetSelected("Local input device")
	if !u.in.host.Disabled() || u.inDevice.Disabled() {
		t.Error("device source: SRT fields should be disabled, input device enabled")
	}
	u.codec.SetSelected("PCM (SMPTE 302M)")
	if !u.bitrate.Disabled() {
		t.Error("bitrate should be disabled for PCM")
	}
	u.send.SetChecked(false)
	if !u.out.port.Disabled() || !u.codec.Disabled() {
		t.Error("output fields should be disabled when not sending")
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
