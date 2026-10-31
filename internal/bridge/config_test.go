package bridge

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := []struct {
		ep    SrtEndpoint
		extra [][2]string
		mask  bool
		want  string
	}{
		{SrtEndpoint{Mode: "listener", Port: 9000, LatencyMs: 120}, nil, false,
			"srt://0.0.0.0:9000?mode=listener&latency=120000&pkt_size=1316"},
		{SrtEndpoint{Mode: "listener", Port: 9000, LatencyMs: 120}, [][2]string{{"timeout", "5000000"}}, false,
			"srt://0.0.0.0:9000?mode=listener&latency=120000&pkt_size=1316&timeout=5000000"},
		{SrtEndpoint{Mode: "caller", Host: " 10.0.0.5 ", Port: 9001, LatencyMs: 200, StreamID: "live/program",
			Passphrase: "secret pass&word"}, nil, false,
			"srt://10.0.0.5:9001?mode=caller&latency=200000&pkt_size=1316&streamid=live%2Fprogram&passphrase=secret%20pass%26word"},
		{SrtEndpoint{Mode: "caller", Host: "h", Port: 1, Passphrase: "0123456789"}, nil, true,
			"srt://h:1?mode=caller&latency=0&pkt_size=1316&passphrase=%2A%2A%2A%2A"},
		{SrtEndpoint{Mode: "caller", Host: "::1", Port: 9001}, nil, false,
			"srt://[::1]:9001?mode=caller&latency=0&pkt_size=1316"},
	}
	for _, c := range cases {
		if got := c.ep.URL(c.extra, c.mask); got != c.want {
			t.Errorf("URL(%+v)\n got %s\nwant %s", c.ep, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := SrtEndpoint{Mode: "caller", Host: "h", Port: 9000, LatencyMs: 120}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]SrtEndpoint{
		"unknown mode":     {Mode: "push", Host: "h", Port: 1},
		"host is required": {Mode: "caller", Port: 1},
		"port must be":     {Mode: "listener", Port: 0},
		"negative":         {Mode: "listener", Port: 1, LatencyMs: -1},
		"10-79":            {Mode: "listener", Port: 1, Passphrase: "short"},
	}
	for want, ep := range bad {
		if err := ep.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%+v) = %v, want error containing %q", ep, err, want)
		}
	}
}

func TestMaskSecrets(t *testing.T) {
	in := `-i 'srt://h:1?mode=caller&passphrase=topsecret123&streamid=x' passphrase=abc`
	want := `-i 'srt://h:1?mode=caller&passphrase=****&streamid=x' passphrase=****`
	if got := MaskSecrets(in); got != want {
		t.Errorf("got %s", got)
	}
}

func TestSaveLoadDropsPassphrases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	cfg := DefaultConfig()
	cfg.SrtIn.Passphrase = "0123456789"
	cfg.Codec = "Opus"
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", st.Mode().Perm())
		}
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SrtIn.Passphrase != "" || got.Codec != "Opus" {
		t.Errorf("got passphrase %q codec %q", got.SrtIn.Passphrase, got.Codec)
	}
	cfg.SavePassphrases = true
	_ = SaveConfig(cfg, path)
	if got, _ = LoadConfig(path); got.SrtIn.Passphrase != "0123456789" {
		t.Errorf("passphrase not saved with SavePassphrases")
	}
}

// Settings files written by the earlier Python version must keep working.
func TestLoadPythonSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	py := `{"ffmpeg": "/usr/bin/ffmpeg", "source": "device",
	  "srt_in": {"mode": "caller", "host": "a", "port": 9100, "latency_ms": 300, "passphrase": "", "streamid": ""},
	  "host_api": "Windows WASAPI", "monitor_device": "Speakers (Realtek)  [Windows WASAPI]",
	  "monitor_volume": 0.5, "codec": "MP2", "some_future_key": 1}`
	if err := os.WriteFile(path, []byte(py), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SrtIn.Port != 9100 || cfg.MonitorVolume != 0.5 || cfg.Codec != "MP2" {
		t.Errorf("loaded %+v", cfg)
	}
	if !cfg.SendEnabled || cfg.BitrateK != 192 || cfg.SrtOut.Port != 9001 { // missing keys keep defaults
		t.Errorf("defaults lost: %+v", cfg)
	}
	if cfg.MonitorSource != FromDevice || cfg.SendSource != FromDevice || cfg.Source != "" {
		t.Errorf(`"source": "device" should route both outputs to the device: %+v`, cfg)
	}
	devs := []AudioDevice{{Name: "Mic"}, {Name: "Speakers (Realtek)"}}
	if d := matchDevice(devs, cfg.MonitorDevice); d == nil || d.Name != "Speakers (Realtek)" {
		t.Errorf("old device label not matched: %v", d)
	}
}

// Routing: an older file's single "source" fills both outputs, new files keep
// their own routing, and a file without either gets the talkback defaults.
func TestRoutingMigration(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ json, monitor, send string }{
		{`{"source": "srt"}`, FromSRT, FromSRT}, // relay, as before
		{`{"source": "device"}`, FromDevice, FromDevice},
		{`{"monitor_source": "srt", "send_source": "device"}`, FromSRT, FromDevice},
		{`{"source": "srt", "monitor_source": "srt", "send_source": "device"}`, FromSRT, FromDevice},
		{`{"codec": "MP2"}`, FromSRT, FromDevice},
	} {
		path := filepath.Join(dir, "cfg.json")
		_ = os.WriteFile(path, []byte(c.json), 0o600)
		cfg, err := LoadConfig(path)
		if err != nil || cfg.MonitorSource != c.monitor || cfg.SendSource != c.send {
			t.Errorf("%s: monitor %q send %q err %v, want %q %q", c.json, cfg.MonitorSource, cfg.SendSource, err, c.monitor, c.send)
		}
		_ = SaveConfig(cfg, path)
		if data, _ := os.ReadFile(path); strings.Contains(string(data), `"source"`) {
			t.Errorf("%s: saved file still has the old key", c.json)
		}
	}
}

func TestLoadMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	if cfg, err := LoadConfig(filepath.Join(dir, "nope.json")); err != nil || cfg.Codec != "AAC" {
		t.Errorf("missing file: %v %+v", err, cfg)
	}
	broken := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)
	if cfg, err := LoadConfig(broken); err == nil || cfg.Codec != "AAC" {
		t.Errorf("broken file: %v %+v", err, cfg)
	}
}
