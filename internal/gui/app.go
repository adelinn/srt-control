// Package gui is the desktop interface (Fyne). All engine work lives in
// package bridge; this package only turns widgets into a bridge.Config and
// shows the engine's status, meters and log.
package gui

import (
	_ "embed"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/adelinn/srt-control/internal/bridge"
)

//go:embed icon.png
var iconPNG []byte

const (
	appName       = "SRT Audio Bridge"
	defaultDevice = "System default"
	pollInterval  = 50 * time.Millisecond
	maxLogLines   = 500
)

var (
	latencyChoices = []string{"20", "50", "80", "120", "200", "300", "500", "1000", "2000"}
	bufferChoices  = []string{"20", "40", "60", "80", "100", "150", "200", "300", "500", "1000"}
	sourceLabels   = []string{"SRT stream", "Local input device"}
	sourceKeys     = []string{"srt", "device"}
)

// srtForm is the set of fields for one SRT endpoint, with a hint and a
// preview of the URL ffmpeg will get (passphrase masked).
type srtForm struct {
	mode     *widget.Select
	host     *widget.Entry
	port     *widget.Entry
	latency  *widget.SelectEntry
	pass     *widget.Entry
	streamID *widget.Entry
	hint     *mutedText
	url      *widget.Label
	box      fyne.CanvasObject
}

func newSrtForm() *srtForm {
	f := &srtForm{
		mode:     widget.NewSelect(bridge.SRTModes, nil),
		host:     widget.NewEntry(),
		port:     widget.NewEntry(),
		latency:  widget.NewSelectEntry(latencyChoices),
		pass:     widget.NewPasswordEntry(),
		streamID: widget.NewEntry(),
		hint:     newMutedText("", false),
		url:      widget.NewLabel(""),
	}
	f.url.SizeName = theme.SizeNameCaptionText
	f.url.TextStyle = fyne.TextStyle{Monospace: true}
	f.url.Truncation = fyne.TextTruncateEllipsis
	f.url.Selectable = true

	left := container.New(layout.NewFormLayout(),
		widget.NewLabel("Mode"), f.mode,
		widget.NewLabel("Host"), f.host,
		widget.NewLabel("Passphrase"), f.pass)
	right := container.New(layout.NewFormLayout(),
		widget.NewLabel("Latency"), container.NewBorder(nil, nil, nil, widget.NewLabel("ms"), withWidth(f.latency, 82)),
		widget.NewLabel("Port"), withWidth(f.port, 82),
		widget.NewLabel("Stream ID"), withWidth(f.streamID, 82))
	f.box = container.NewVBox(container.NewBorder(nil, nil, nil, right, left), f.hint, f.url)

	changed := func(string) { f.updatePreview() }
	f.mode.OnChanged = changed
	f.host.OnChanged = changed
	f.port.OnChanged = changed
	f.latency.OnChanged = changed
	f.pass.OnChanged = changed
	f.streamID.OnChanged = changed
	return f
}

func (f *srtForm) endpoint() (bridge.SrtEndpoint, error) {
	num := func(name, s string) (int, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, nil
		}
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number", name)
		}
		return v, nil
	}
	port, err := num("port", f.port.Text)
	if err != nil {
		return bridge.SrtEndpoint{}, err
	}
	latency, err := num("latency", f.latency.Text)
	if err != nil {
		return bridge.SrtEndpoint{}, err
	}
	return bridge.SrtEndpoint{
		Mode: f.mode.Selected, Host: strings.TrimSpace(f.host.Text), Port: port, LatencyMs: latency,
		Passphrase: f.pass.Text, StreamID: strings.TrimSpace(f.streamID.Text),
	}, nil
}

func (f *srtForm) set(ep bridge.SrtEndpoint) {
	f.mode.SetSelected(ep.Mode)
	f.host.SetText(ep.Host)
	f.port.SetText(strconv.Itoa(ep.Port))
	f.latency.SetText(strconv.Itoa(ep.LatencyMs))
	f.pass.SetText(ep.Passphrase)
	f.streamID.SetText(ep.StreamID)
	f.updatePreview()
}

func (f *srtForm) updatePreview() {
	f.hint.SetText(bridge.ModeHints[f.mode.Selected])
	if f.mode.Selected == "listener" {
		f.host.SetPlaceHolder("blank = all interfaces")
	} else {
		f.host.SetPlaceHolder("hostname or IP")
	}
	ep, err := f.endpoint()
	if err == nil {
		err = ep.Validate()
	}
	if err != nil {
		f.url.Importance = widget.WarningImportance
		f.url.SetText("Not valid yet: " + err.Error())
	} else {
		f.url.Importance = widget.MediumImportance
		f.url.SetText(ep.URL(nil, true))
	}
}

func (f *srtForm) setEnabled(on bool) {
	for _, w := range []fyne.Disableable{f.mode, f.host, f.port, f.latency, f.pass, f.streamID} {
		enable(w, on)
	}
}

func enable(w fyne.Disableable, on bool) {
	if on {
		w.Enable()
	} else {
		w.Disable()
	}
}

type ui struct {
	app     fyne.App
	win     fyne.Window
	cfgPath string
	cfg     bridge.Config
	engine  *bridge.Engine

	logMu     sync.Mutex
	pending   []string // written from any goroutine, drained by poll
	logLines  []string
	logView   *widget.Label
	logScroll *container.Scroll

	meter   *levelMeter
	meterDB [bridge.Channels]float64
	tick    int

	btnStart, btnStop *widget.Button
	badge             *stateBadge
	api               *widget.Select
	source            *widget.RadioGroup
	in, out           *srtForm
	lowLatency        *widget.Check
	inDevice          *widget.Select
	send              *widget.Check
	codec             *widget.Select
	bitrate           *widget.SelectEntry
	keepalive         *widget.Check
	monitor           *widget.Check
	outDevice         *widget.Select
	volume            *widget.Slider
	volumeLabel       *widget.Label
	mute              *widget.Check
	buffer            *widget.SelectEntry
	ffmpeg            *widget.Entry
	ffInfo            *widget.Label
	savePass          *widget.Check
	lightSource       *statusLight
	lightMonitor      *statusLight
	lightSender       *statusLight

	bufferTimer  *time.Timer
	quit         chan struct{} // closed when the window closes or a signal arrives
	quitOnce     sync.Once
	shutdownOnce sync.Once
}

func (u *ui) closing() bool {
	select {
	case <-u.quit:
		return true
	default:
		return false
	}
}

// Run opens the window and blocks until it is closed.
func Run(cfgPath string) {
	a := app.NewWithID("io.github.adelinn.srt-audio-bridge")
	a.Settings().SetTheme(bridgeTheme{})
	a.SetIcon(fyne.NewStaticResource("icon.png", iconPNG))
	u := &ui{app: a, cfgPath: cfgPath, meterDB: [bridge.Channels]float64{-90, -90}, quit: make(chan struct{})}
	u.win = a.NewWindow(appName)
	cfg, err := bridge.LoadConfig(cfgPath)
	if err != nil {
		u.log("Could not read settings (" + err.Error() + "); using defaults")
	}
	u.cfg = cfg
	u.win.SetContent(u.build())
	u.fill()
	u.refreshDevices(false)
	u.checkFFmpeg()
	if len(bridge.AudioBackends()) == 0 {
		u.log("No audio system found. SRT-to-SRT still works with monitoring off.")
	}
	u.win.SetCloseIntercept(u.onClose)
	u.win.Resize(fyne.NewSize(1040, 820))
	a.Lifecycle().SetOnStarted(func() {
		go func() {
			t := time.NewTicker(pollInterval)
			defer t.Stop()
			for {
				select {
				case <-u.quit:
					return
				case <-t.C:
					fyne.Do(u.poll)
				}
			}
		}()
		// Fyne quits its loop on SIGINT/SIGTERM by itself; stop touching
		// widgets right away, shutdown() below does the rest.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			select {
			case <-sig:
				u.markClosing()
			case <-u.quit:
			}
		}()
	})
	u.win.ShowAndRun()
	u.shutdown() // the loop also ends on a signal, so ffmpeg is stopped here too
}

func (u *ui) markClosing() { u.quitOnce.Do(func() { close(u.quit) }) }

// shutdown stops the engine (and so every ffmpeg) and saves the settings, once.
func (u *ui) shutdown() {
	u.shutdownOnce.Do(func() {
		u.markClosing()
		if u.bufferTimer != nil {
			u.bufferTimer.Stop()
		}
		if u.engine != nil {
			u.engine.Stop()
			u.engine = nil
		}
		if cfg, err := u.collect(); err == nil {
			_ = bridge.SaveConfig(cfg, u.cfgPath)
		}
	})
}

// log is safe to call from any goroutine.
func (u *ui) log(msg string) {
	line := time.Now().Format("15:04:05") + "  " + bridge.MaskSecrets(msg)
	u.logMu.Lock()
	u.pending = append(u.pending, line)
	u.logMu.Unlock()
}

func (u *ui) build() fyne.CanvasObject {
	// toolbar
	u.btnStart = widget.NewButtonWithIcon("Start", theme.MediaPlayIcon(), u.start)
	u.btnStart.Importance = widget.HighImportance
	u.btnStop = widget.NewButtonWithIcon("Stop", theme.MediaStopIcon(), u.stop)
	u.btnStop.Disable()
	u.badge = newStateBadge()
	u.api = widget.NewSelect(nil, func(string) { u.fillDeviceLists() })
	rescan := widget.NewButtonWithIcon("Devices", theme.ViewRefreshIcon(), func() { u.refreshDevices(true) })
	toolbar := container.NewBorder(nil, nil,
		container.New(layout.NewCustomPaddedHBoxLayout(8), withWidth(u.btnStart, 108), u.btnStop, u.badge),
		container.NewHBox(widget.NewLabel("Audio API"), withWidth(u.api, 150), rescan))

	// source
	u.in = newSrtForm()
	u.source = widget.NewRadioGroup(sourceLabels, func(string) { u.updateEnabled() })
	u.source.Horizontal = true
	u.source.Required = true
	u.lowLatency = widget.NewCheck("Low-latency start (smaller probe)", nil)
	u.inDevice = widget.NewSelect(nil, nil)
	src := newPanel("Source", nil, container.NewVBox(
		u.source, u.in.box, u.lowLatency,
		container.NewBorder(nil, nil, widget.NewLabel("Input device"), nil, u.inDevice)))

	// destination
	u.out = newSrtForm()
	u.send = widget.NewCheck("Send over SRT", func(bool) { u.updateEnabled() })
	u.codec = widget.NewSelect(bridge.Codecs, func(string) { u.updateEnabled() })
	bitrates := make([]string, len(bridge.Bitrates))
	for i, b := range bridge.Bitrates {
		bitrates[i] = strconv.Itoa(b)
	}
	u.bitrate = widget.NewSelectEntry(bitrates)
	u.keepalive = widget.NewCheck("Send silence while the source is down (keeps the link up)", nil)
	enc := container.NewHBox(widget.NewLabel("Codec"), withWidth(u.codec, 170),
		widget.NewLabel("Bitrate"), withWidth(u.bitrate, 82), widget.NewLabel("kbit/s"))
	dst := newPanel("SRT output", u.send, container.NewVBox(u.out.box, enc, u.keepalive))

	// monitor
	u.monitor = widget.NewCheck("Monitor", func(bool) { u.monitorChanged() })
	u.outDevice = widget.NewSelect(nil, func(string) { u.monitorChanged() })
	u.volume = widget.NewSlider(0, 1.5)
	u.volume.Step = 0.01
	u.volume.OnChanged = func(float64) { u.volumeChanged() }
	u.volumeLabel = widget.NewLabel("100%")
	u.mute = widget.NewCheck("Mute", func(bool) { u.volumeChanged() })
	u.buffer = widget.NewSelectEntry(bufferChoices)
	u.buffer.OnChanged = func(string) { u.bufferChanged() }
	mon := newPanel("Monitor", nil, container.NewBorder(nil, nil,
		container.NewHBox(u.monitor, widget.NewLabel("Output")),
		container.NewHBox(widget.NewLabel("Volume"), withWidth(u.volume, 150), withWidth(u.volumeLabel, 46),
			u.mute, widget.NewLabel("Buffer"), withWidth(u.buffer, 82), widget.NewLabel("ms")),
		u.outDevice))

	// levels and status
	u.meter = newLevelMeter()
	u.lightSource, u.lightMonitor, u.lightSender = newStatusLight("Source"), newStatusLight("Monitor"), newStatusLight("SRT out")
	levels := newPanel("Levels & status", nil, container.NewVBox(u.meter,
		container.New(columns{gap: 12}, u.lightSource.box, u.lightMonitor.box, u.lightSender.box)))

	// settings
	u.ffmpeg = widget.NewEntry()
	u.ffmpeg.SetPlaceHolder("ffmpeg (from PATH or next to this program)")
	u.ffInfo = widget.NewLabel("")
	u.savePass = widget.NewCheck("Save passphrases", nil)
	settings := newPanel("Settings", nil, container.NewBorder(nil, nil, widget.NewLabel("ffmpeg"),
		container.NewHBox(
			widget.NewButtonWithIcon("Browse…", theme.FolderOpenIcon(), u.browseFFmpeg),
			widget.NewButton("Check", u.checkFFmpeg),
			withWidth(u.ffInfo, 150), u.savePass),
		u.ffmpeg))

	// log
	u.logView = widget.NewLabel("")
	u.logView.TextStyle = fyne.TextStyle{Monospace: true}
	u.logView.SizeName = theme.SizeNameCaptionText
	u.logView.Wrapping = fyne.TextWrapWord
	u.logView.Selectable = true
	u.logScroll = container.NewVScroll(u.logView)
	u.logScroll.SetMinSize(fyne.NewSize(0, 90))
	copyLog := widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), func() {
		u.app.Clipboard().SetContent(strings.Join(u.logLines, "\n"))
	})
	copyLog.Importance = widget.LowImportance
	logPanel := newPanel("Log", copyLog, u.logScroll)

	content := container.New(vstack{gap: 8},
		toolbar, container.New(columns{gap: 8}, src, dst), mon, levels, settings, logPanel)
	// Scrolls only when the window is smaller than the layout (small or
	// high-DPI laptop screens); otherwise the log takes the spare height.
	return container.NewVScroll(container.New(layout.NewCustomPaddedLayout(10, 12, 12, 12), content))
}

func (u *ui) sourceKey() string {
	for i, l := range sourceLabels {
		if u.source.Selected == l {
			return sourceKeys[i]
		}
	}
	return "srt"
}

// fill puts the settings into the widgets.
func (u *ui) fill() {
	c := u.cfg
	u.source.SetSelected(sourceLabels[0])
	if c.Source == "device" {
		u.source.SetSelected(sourceLabels[1])
	}
	u.in.set(c.SrtIn)
	u.out.set(c.SrtOut)
	u.lowLatency.SetChecked(c.LowLatencyInput)
	u.send.SetChecked(c.SendEnabled)
	u.codec.SetSelected("AAC")
	u.codec.SetSelected(c.Codec) // ignored if unknown
	u.bitrate.SetText(strconv.Itoa(c.BitrateK))
	u.keepalive.SetChecked(c.KeepaliveSilence)
	u.monitor.SetChecked(c.MonitorEnabled)
	u.volume.SetValue(c.MonitorVolume)
	u.mute.SetChecked(c.MonitorMuted)
	u.buffer.SetText(strconv.Itoa(c.MonitorBufferMs))
	u.ffmpeg.SetText(c.FFmpeg)
	u.savePass.SetChecked(c.SavePassphrases)
	u.volumeChanged()
	u.updateEnabled()
}

// collect turns the widgets into settings, or says what is wrong.
func (u *ui) collect() (bridge.Config, error) {
	asInt := func(s, name string, lo, hi int) (int, error) {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number", name)
		}
		if n := int(v); n >= lo && n <= hi {
			return n, nil
		}
		return 0, fmt.Errorf("%s must be %d-%d", name, lo, hi)
	}
	in, err := u.in.endpoint()
	if err != nil {
		return bridge.Config{}, fmt.Errorf("SRT in: %w", err)
	}
	out, err := u.out.endpoint()
	if err != nil {
		return bridge.Config{}, fmt.Errorf("SRT out: %w", err)
	}
	buffer, err := asInt(u.buffer.Text, "Monitor buffer", 20, 1000)
	if err != nil {
		return bridge.Config{}, err
	}
	bitrate, err := asInt(u.bitrate.Text, "Bitrate", 16, 512)
	if err != nil {
		return bridge.Config{}, err
	}
	ffmpeg := strings.TrimSpace(u.ffmpeg.Text)
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	return bridge.Config{
		FFmpeg:           ffmpeg,
		Source:           u.sourceKey(),
		SrtIn:            in,
		LowLatencyInput:  u.lowLatency.Checked,
		HostAPI:          u.api.Selected,
		InputDevice:      deviceValue(u.inDevice.Selected),
		MonitorEnabled:   u.monitor.Checked,
		MonitorDevice:    deviceValue(u.outDevice.Selected),
		MonitorVolume:    math.Round(u.volume.Value*1000) / 1000,
		MonitorMuted:     u.mute.Checked,
		MonitorBufferMs:  buffer,
		SendEnabled:      u.send.Checked,
		SrtOut:           out,
		Codec:            u.codec.Selected,
		BitrateK:         bitrate,
		KeepaliveSilence: u.keepalive.Checked,
		SavePassphrases:  u.savePass.Checked,
	}, nil
}

func deviceValue(label string) string {
	if label == defaultDevice {
		return ""
	}
	return label
}

func (u *ui) updateEnabled() {
	srt := u.sourceKey() == "srt"
	u.in.setEnabled(srt)
	enable(u.lowLatency, srt)
	enable(u.inDevice, !srt)
	send := u.send.Checked
	u.out.setEnabled(send)
	enable(u.codec, send)
	enable(u.keepalive, send)
	enable(u.bitrate, send && !bridge.IsLossless(u.codec.Selected))
}

// -- devices -------------------------------------------------------------

func (u *ui) refreshDevices(announce bool) {
	backends := bridge.AudioBackends()
	u.api.Options = backends
	wanted := u.api.Selected
	if wanted == "" {
		wanted = u.cfg.HostAPI
	}
	if !contains(backends, wanted) {
		wanted = ""
		if len(backends) > 0 {
			wanted = backends[0]
		}
	}
	enable(u.api, len(backends) > 0)
	u.api.SetSelected(wanted)
	u.api.Refresh()
	n := u.fillDeviceLists()
	if announce {
		u.log(fmt.Sprintf("Found %d audio devices (%s)", n, wanted))
	}
}

func (u *ui) fillDeviceLists() int {
	total := 0
	api := u.api.Selected
	for _, d := range []struct {
		sel     *widget.Select
		capture bool
		saved   string
	}{{u.inDevice, true, u.cfg.InputDevice}, {u.outDevice, false, u.cfg.MonitorDevice}} {
		opts := []string{defaultDevice}
		if api != "" {
			devs, err := bridge.ListDevices(api, d.capture)
			if err != nil {
				u.log("Listing devices failed: " + err.Error())
			}
			for _, dev := range devs {
				opts = append(opts, dev.Name)
			}
			total += len(devs)
		}
		current := d.sel.Selected
		if current == "" {
			current = strings.SplitN(d.saved, "  [", 2)[0] // drop the Python-era "  [API]" suffix
		}
		if !contains(opts, current) {
			current = defaultDevice
		}
		d.sel.Options = opts
		d.sel.SetSelected(current)
		d.sel.Refresh()
	}
	return total
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// -- ffmpeg ----------------------------------------------------------------

func (u *ui) browseFFmpeg() {
	picked := func(path string) {
		u.ffmpeg.SetText(path)
		u.checkFFmpeg()
	}
	if nativeOpenFile(u.win, "Select the ffmpeg binary", picked) {
		return
	}
	dialog.ShowFileOpen(func(r fyne.URIReadCloser, err error) {
		if err != nil || r == nil {
			return
		}
		path := r.URI().Path()
		_ = r.Close()
		picked(path)
	}, u.win)
}

// checkFFmpeg probes ffmpeg in the background so the window stays responsive.
func (u *ui) checkFFmpeg() {
	hint := u.ffmpeg.Text
	setInfo := func(text string, imp widget.Importance) {
		fyne.Do(func() {
			u.ffInfo.Importance = imp
			u.ffInfo.SetText(text)
		})
	}
	u.ffInfo.Importance = widget.MediumImportance
	u.ffInfo.SetText("checking…")
	go func() {
		path := bridge.FindFFmpeg(hint)
		if path == "" {
			setInfo("not found", widget.DangerImportance)
			u.log("ffmpeg not found. Install it, put it next to this program, or set its path.")
			return
		}
		info, err := bridge.ProbeFFmpeg(path)
		if err != nil {
			setInfo("won't run", widget.DangerImportance)
			u.log("ffmpeg check failed: " + err.Error())
			return
		}
		version := info.Version
		if len(version) > 18 {
			version = version[:18]
		}
		if info.SRT {
			setInfo(version+" · SRT OK", widget.SuccessImportance)
		} else {
			setInfo(version+" · no SRT", widget.DangerImportance)
			u.log("This ffmpeg build has no SRT support. Use a build with libsrt " +
				"(gyan.dev or BtbN on Windows, Homebrew on macOS, distro ffmpeg on most Linux).")
		}
		var missing []string
		for _, c := range bridge.Codecs {
			if !info.Encoders[bridge.CodecEncoder(c)] {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			u.log("Codecs not in this ffmpeg build: " + strings.Join(missing, ", "))
		}
		u.log("ffmpeg " + info.Version + " at " + path)
	}()
}

// -- run control -------------------------------------------------------------

func (u *ui) showMessage(msg string, isError bool) {
	if nativeMessage(u.win, appName, msg, isError) {
		return
	}
	if isError {
		dialog.ShowError(errors.New(msg), u.win)
	} else {
		dialog.ShowInformation(appName, msg, u.win)
	}
}

func (u *ui) start() {
	cfg, err := u.collect()
	if err != nil {
		u.showMessage(err.Error(), false)
		return
	}
	if u.engine != nil {
		u.engine.Stop()
		u.engine = nil
		u.log("Restarting with new settings")
	}
	u.cfg = cfg
	if err := bridge.SaveConfig(cfg, u.cfgPath); err != nil {
		u.log("Could not save settings: " + err.Error())
	}
	e := bridge.NewEngine(cfg, u.log)
	if err := e.Start(); err != nil {
		u.setRunning(false)
		u.log("Start failed: " + err.Error())
		u.showMessage(err.Error(), true)
		return
	}
	u.engine = e
	u.setRunning(true)
	u.log("Started")
}

func (u *ui) stop() {
	if u.engine != nil {
		u.engine.Stop()
		u.engine = nil
		u.log("Stopped")
	}
	u.setRunning(false)
}

func (u *ui) setRunning(running bool) {
	if running {
		u.btnStart.SetText("Restart")
		u.btnStart.SetIcon(theme.MediaReplayIcon())
		u.btnStop.Enable()
		u.btnStop.Importance = widget.DangerImportance
	} else {
		u.btnStart.SetText("Start")
		u.btnStart.SetIcon(theme.MediaPlayIcon())
		u.btnStop.Disable()
		u.btnStop.Importance = widget.MediumImportance
	}
	u.btnStop.Refresh()
	u.updateStatus()
}

func (u *ui) monitorChanged() {
	if u.engine == nil || !u.engine.Running() {
		return
	}
	buffer, err := strconv.Atoi(strings.TrimSpace(u.buffer.Text))
	if err != nil {
		buffer = u.cfg.MonitorBufferMs
	}
	buffer = min(1000, max(20, buffer))
	if err := u.engine.SetMonitor(u.monitor.Checked, u.api.Selected, deviceValue(u.outDevice.Selected), buffer); err != nil {
		u.log("Monitor: " + err.Error())
	}
}

// bufferChanged waits for typing to pause before reopening the device.
func (u *ui) bufferChanged() {
	if u.bufferTimer != nil {
		u.bufferTimer.Stop()
	}
	u.bufferTimer = time.AfterFunc(600*time.Millisecond, func() { fyne.Do(u.monitorChanged) })
}

func (u *ui) volumeChanged() {
	u.volumeLabel.SetText(fmt.Sprintf("%d%%", int(math.Round(u.volume.Value*100))))
	if u.engine != nil {
		u.engine.SetVolume(math.Round(u.volume.Value*1000)/1000, u.mute.Checked)
	}
}

// -- periodic update -----------------------------------------------------------

func (u *ui) poll() {
	if u.closing() {
		return
	}
	u.logMu.Lock()
	lines := u.pending
	u.pending = nil
	u.logMu.Unlock()
	if len(lines) > 0 {
		u.logLines = append(u.logLines, lines...)
		if over := len(u.logLines) - maxLogLines; over > 0 {
			u.logLines = append([]string(nil), u.logLines[over:]...)
		}
		u.logView.SetText(strings.Join(u.logLines, "\n"))
		u.logScroll.ScrollToBottom()
	}

	var peaks [bridge.Channels]float64
	if u.engine != nil {
		peaks = u.engine.TakePeaks()
	}
	for i, p := range peaks {
		db := 20 * math.Log10(max(p, 1e-5))
		u.meterDB[i] = max(db, u.meterDB[i]-1.2) // ~24 dB/s fall-back
	}
	u.meter.setLevels(u.meterDB)

	if u.tick++; u.tick%5 == 0 {
		u.updateStatus()
	}
}

func (u *ui) updateStatus() {
	off := bridge.ComponentStatus{State: "off"}
	st := bridge.Status{Source: off, Monitor: off, Sender: off}
	if u.engine != nil {
		st = u.engine.Snapshot()
	}
	u.lightSource.set(st.Source.State, st.Source.Detail)
	u.lightMonitor.set(st.Monitor.State, st.Monitor.Detail)
	u.lightSender.set(st.Sender.State, st.Sender.Detail)

	// The badge follows the signal path; the monitor only counts when it fails.
	switch {
	case u.engine == nil:
		u.badge.set("STOPPED", theme.ColorNameDisabled)
	case st.Source.State == "error" || st.Sender.State == "error" || st.Monitor.State == "error":
		u.badge.set("ERROR", theme.ColorNameError)
	case st.Source.State == "no audio":
		u.badge.set("NO AUDIO", theme.ColorNameWarning)
	case goodStates[st.Source.State] && (st.Sender.State == "off" || goodStates[st.Sender.State]):
		u.badge.set("LIVE", theme.ColorNameSuccess)
	default:
		u.badge.set("CONNECTING", theme.ColorNameWarning)
	}
}

func (u *ui) onClose() {
	u.shutdown()
	u.win.Close()
}
