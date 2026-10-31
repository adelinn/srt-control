package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Audio format used everywhere inside the bridge: 48 kHz, s16le, stereo.
const (
	Rate        = 48_000
	Channels    = 2
	FrameBytes  = Channels * 2 // s16le stereo
	BlockFrames = 480          // 10 ms
	BlockBytes  = BlockFrames * FrameBytes

	maxAhead       = 0.20                   // seconds: audio arriving faster than real time beyond this is dropped
	keepaliveAfter = 500 * time.Millisecond // source silent this long -> send silence to keep SRT out alive
)

// Codecs offered for the SRT output, in display order.
var Codecs = []string{"AAC", "MP2", "Opus", "PCM (SMPTE 302M)"}

var codecArgs = map[string][]string{
	"AAC":              {"-c:a", "aac"},
	"MP2":              {"-c:a", "mp2"},
	"Opus":             {"-c:a", "libopus"},
	"PCM (SMPTE 302M)": {"-c:a", "s302m", "-strict", "-2"},
}

// CodecEncoder returns the ffmpeg encoder name behind a codec label.
func CodecEncoder(codec string) string {
	if a, ok := codecArgs[codec]; ok {
		return a[1]
	}
	return ""
}

// IsLossless reports whether a codec ignores the bitrate setting.
func IsLossless(codec string) bool { return codec == "PCM (SMPTE 302M)" }

// Bitrates offered in the GUI, in kbit/s.
var Bitrates = []int{96, 128, 160, 192, 256, 320, 384}

// SRTModes are the connection modes ffmpeg's libsrt supports.
var SRTModes = []string{"caller", "listener", "rendezvous"}

// ModeHints explain each SRT mode in one line.
var ModeHints = map[string]string{
	"caller":     "Connects out to host:port.",
	"listener":   "Waits for the other side to connect. Host blank = all interfaces.",
	"rendezvous": "Both sides connect to each other at the same time.",
}

var secretRE = regexp.MustCompile(`(passphrase=)[^&\s'"]+`)

// MaskSecrets hides SRT passphrases in text that goes to a log.
func MaskSecrets(text string) string { return secretRE.ReplaceAllString(text, "${1}****") }

// SrtEndpoint is one side of an SRT connection.
type SrtEndpoint struct {
	Mode       string `json:"mode"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	LatencyMs  int    `json:"latency_ms"`
	Passphrase string `json:"passphrase"`
	StreamID   string `json:"streamid"`
}

// Validate checks the endpoint before ffmpeg gets it.
func (e SrtEndpoint) Validate() error {
	known := false
	for _, m := range SRTModes {
		known = known || m == e.Mode
	}
	switch {
	case !known:
		return fmt.Errorf("unknown mode '%s'", e.Mode)
	case e.Mode != "listener" && strings.TrimSpace(e.Host) == "":
		return fmt.Errorf("host is required in %s mode", e.Mode)
	case e.Port < 1 || e.Port > 65535:
		return errors.New("port must be 1-65535")
	case e.LatencyMs < 0:
		return errors.New("latency can't be negative")
	case e.Passphrase != "" && (len(e.Passphrase) < 10 || len(e.Passphrase) > 79):
		return errors.New("SRT passphrases must be 10-79 characters")
	}
	return nil
}

// URL builds the ffmpeg srt:// URL. extra holds additional key/value pairs;
// mask replaces the passphrase with ****.
func (e SrtEndpoint) URL(extra [][2]string, mask bool) string {
	host := strings.TrimSpace(e.Host)
	if host == "" {
		host = "0.0.0.0"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") { // bare IPv6 address
		host = "[" + host + "]"
	}
	params := [][2]string{
		{"mode", e.Mode},
		{"latency", strconv.Itoa(e.LatencyMs * 1000)}, // ffmpeg wants microseconds
		{"pkt_size", "1316"},                          // 7 x 188-byte TS packets
	}
	params = append(params, extra...)
	if e.StreamID != "" {
		params = append(params, [2]string{"streamid", e.StreamID})
	}
	if e.Passphrase != "" {
		p := e.Passphrase
		if mask {
			p = "****"
		}
		params = append(params, [2]string{"passphrase", p})
	}
	q := make([]string, len(params))
	for i, kv := range params {
		q[i] = kv[0] + "=" + escape(kv[1])
	}
	return fmt.Sprintf("srt://%s:%d?%s", host, e.Port, strings.Join(q, "&"))
}

// escape percent-encodes everything except RFC 3986 unreserved characters.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Inputs an output can take its audio from.
const (
	FromSRT    = "srt"    // the SRT input
	FromDevice = "device" // the local input device (mic, line-in)
)

// Config holds every setting. The JSON layout extends the earlier Python
// version's, so existing settings files keep working.
type Config struct {
	FFmpeg string `json:"ffmpeg"`
	// Source is the pre-routing setting (one input feeding both outputs). It
	// is only read to fill MonitorSource and SendSource, then dropped.
	Source           string      `json:"source,omitempty"`
	MonitorSource    string      `json:"monitor_source"` // FromSRT | FromDevice: what the monitor plays
	SendSource       string      `json:"send_source"`    // FromSRT | FromDevice: what SRT out sends
	SrtIn            SrtEndpoint `json:"srt_in"`
	LowLatencyInput  bool        `json:"low_latency_input"`
	HostAPI          string      `json:"host_api"`     // audio backend, "" = default for this OS
	InputDevice      string      `json:"input_device"` // "" = system default
	MonitorEnabled   bool        `json:"monitor_enabled"`
	MonitorDevice    string      `json:"monitor_device"` // "" = system default
	MonitorVolume    float64     `json:"monitor_volume"`
	MonitorMuted     bool        `json:"monitor_muted"`
	MonitorBufferMs  int         `json:"monitor_buffer_ms"`
	SendEnabled      bool        `json:"send_enabled"`
	SrtOut           SrtEndpoint `json:"srt_out"`
	Codec            string      `json:"codec"`
	BitrateK         int         `json:"bitrate_k"`
	KeepaliveSilence bool        `json:"keepalive_silence"`
	SavePassphrases  bool        `json:"save_passphrases"`
}

// DefaultConfig returns the settings used when there is no settings file:
// listen to the SRT input and send the local input (talkback).
func DefaultConfig() Config {
	return Config{
		FFmpeg:           "ffmpeg",
		MonitorSource:    FromSRT,
		SendSource:       FromDevice,
		SrtIn:            SrtEndpoint{Mode: "listener", Port: 9000, LatencyMs: 120},
		LowLatencyInput:  true,
		MonitorEnabled:   true,
		MonitorVolume:    1.0,
		MonitorBufferMs:  100,
		SendEnabled:      true,
		SrtOut:           SrtEndpoint{Mode: "caller", Host: "127.0.0.1", Port: 9001, LatencyMs: 120},
		Codec:            "AAC",
		BitrateK:         192,
		KeepaliveSilence: true,
	}
}

// DefaultConfigPath is ~/.srt_audio_bridge.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".srt_audio_bridge.json")
}

// LoadConfig reads a settings file. A missing file gives the defaults; an
// unreadable one gives the defaults plus the error, so callers can report it.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.MonitorSource, cfg.SendSource = "", ""         // so an older file's "source" can fill them
	if err := json.Unmarshal(data, &cfg); err != nil { // missing keys keep their defaults
		return DefaultConfig(), fmt.Errorf("%s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// normalize fills the routing from an older file's single "source": both
// outputs took their audio from it (for "srt", the relay setup).
func (c *Config) normalize() {
	legacy := c.Source
	c.Source = ""
	valid := func(s string) bool { return s == FromSRT || s == FromDevice }
	if !valid(legacy) {
		legacy = ""
	}
	if !valid(c.MonitorSource) {
		c.MonitorSource = legacy
		if legacy == "" {
			c.MonitorSource = FromSRT
		}
	}
	if !valid(c.SendSource) {
		c.SendSource = legacy
		if legacy == "" {
			c.SendSource = FromDevice
		}
	}
}

// SaveConfig writes the settings with 0600 permissions, because the file can
// hold passphrases. Passphrases are left out unless SavePassphrases is set.
func SaveConfig(cfg Config, path string) error {
	if !cfg.SavePassphrases {
		cfg.SrtIn.Passphrase, cfg.SrtOut.Passphrase = "", ""
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
