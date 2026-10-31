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
	latencyChoices = []string{"20", "30", "40", "60", "80", "120", "200", "300", "500", "1000", "2000"}
	bufferChoices  = []string{"20", "40", "60", "80", "100", "150", "200", "300", "500", "1000"}
	// what the monitor plays and SRT out sends
	routeLabels = []string{"SRT input", "Local input"}
	routeKeys   = []string{bridge.FromSRT, bridge.FromDevice}
)

func routeKey(r *widget.RadioGroup) string {
	if r.Selected == routeLabels[1] {
		return routeKeys[1]
	}
	return routeKeys[0]
}

func setRoute(r *widget.RadioGroup, key string) {
	if key == routeKeys[1] {
		r.SetSelected(routeLabels[1])
	} else {
		r.SetSelected(routeLabels[0])
	}
}

// srtForm is the set of fields for one SRT endpoint, with a hint and a
// preview of the URL ffmpeg will get (passphrase masked). "Edit URL" turns
// the preview into an editable URL that is used exactly as typed.
type srtForm struct {
	mode     *widget.Select
	host     *widget.Entry
	port     *widget.Entry
	latency  *widget.SelectEntry
	pass     *widget.Entry
	streamID *widget.Entry
	hint     *mutedText
	url      *widget.Label
	custom   *widget.Entry
	edit     *widget.Check
	enabled  bool
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
		custom:   widget.NewEntry(),
		enabled:  true,
	}
	f.url.SizeName = theme.SizeNameCaptionText
	f.url.TextStyle = fyne.TextStyle{Monospace: true}
	f.url.Truncation = fyne.TextTruncateEllipsis
	f.url.Selectable = true
	f.custom.TextStyle = fyne.TextStyle{Monospace: true}
	f.custom.SetPlaceHolder("srt://host:port?mode=caller&transtype=live&latency=120000")
	f.custom.Hide()
	f.edit = widget.NewCheck("Edit URL", f.toggleEdit)

	left := container.New(layout.NewFormLayout(),
		widget.NewLabel("Mode"), f.mode,
		widget.NewLabel("Host"), f.host,
		widget.NewLabel("Passphrase"), f.pass)
	right := container.New(layout.NewFormLayout(),
		widget.NewLabel("Latency"), container.NewBorder(nil, nil, nil, widget.NewLabel("ms"), withWidth(f.latency, 82)),
		widget.NewLabel("Port"), withWidth(f.port, 82),
		widget.NewLabel("Stream ID"), withWidth(f.streamID, 82))
	urlRow := container.NewBorder(nil, nil, nil, f.edit, container.NewStack(f.url, f.custom))
	f.box = container.NewVBox(container.NewBorder(nil, nil, nil, right, left), f.hint, urlRow)

	changed := func(string) { f.updatePreview() }
	f.mode.OnChanged = changed
	f.host.OnChanged = changed
	f.port.OnChanged = changed
	f.latency.OnChanged = changed
	f.pass.OnChanged = changed
	f.streamID.OnChanged = changed
	f.custom.OnChanged = changed
	return f
}

func (f *srtForm) endpoint() (bridge.SrtEndpoint, error) {
	custom := f.edit.Checked
	num := func(name, s string) (int, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, nil
		}
		v, err := strconv.Atoi(s)
		if err != nil && !custom { // the fields don't matter while the URL is edited
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
		UseCustomURL: custom, CustomURL: strings.TrimSpace(f.custom.Text),
	}, nil
}

func (f *srtForm) set(ep bridge.SrtEndpoint) {
	f.mode.SetSelected(ep.Mode)
	f.host.SetText(ep.Host)
	f.port.SetText(strconv.Itoa(ep.Port))
	f.latency.SetText(strconv.Itoa(ep.LatencyMs))
	f.pass.SetText(ep.Passphrase)
	f.streamID.SetText(ep.StreamID)
	f.custom.SetText(ep.CustomURL)
	f.edit.SetChecked(ep.UseCustomURL)
	f.applyEnabled()
	f.updatePreview()
}

// toggleEdit starts the editable URL from the one the fields give.
func (f *srtForm) toggleEdit(on bool) {
	if on && strings.TrimSpace(f.custom.Text) == "" {
		f.edit.Checked = false
		if ep, err := f.endpoint(); err == nil {
			f.custom.SetText(ep.URL(nil, false))
		}
		f.edit.Checked = true
	}
	f.applyEnabled()
	f.updatePreview()
}

func (f *srtForm) updatePreview() {
	if f.mode.Selected == "listener" {
		f.host.SetPlaceHolder("blank = all interfaces")
	} else {
		f.host.SetPlaceHolder("hostname or IP")
	}
	ep, err := f.endpoint()
	if err == nil {
		err = ep.Validate()
	}
	if f.edit.Checked {
		if err != nil {
			f.hint.setWarning(true)
			f.hint.SetText("Not valid yet: " + err.Error())
		} else {
			mode, _, _, _ := ep.Target()
			f.hint.setWarning(false)
			f.hint.SetText("Used exactly as typed (" + mode + " mode). The fields above are ignored.")
		}
		return
	}
	f.hint.setWarning(false)
	f.hint.SetText(bridge.ModeHints[f.mode.Selected])
	if err != nil {
		f.url.Importance = widget.WarningImportance
		f.url.SetText("Not valid yet: " + err.Error())
	} else {
		f.url.Importance = widget.MediumImportance
		f.url.SetText(ep.URL(nil, true))
	}
}

func (f *srtForm) setEnabled(on bool) {
	f.enabled = on
	f.applyEnabled()
}

// applyEnabled greys out the fields while the URL is edited by hand, and
// everything while the form as a whole is off.
func (f *srtForm) applyEnabled() {
	custom := f.edit.Checked
	if custom {
		f.url.Hide()
		f.custom.Show()
	} else {
		f.custom.Hide()
		f.url.Show()
	}
	for _, w := range []fyne.Disableable{f.mode, f.host, f.port, f.latency, f.pass, f.streamID} {
		enable(w, f.enabled && !custom)
	}
	enable(f.edit, f.enabled)
	enable(f.custom, f.enabled)
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

	meterSRT, meterInput *levelMeter
	dbSRT, dbInput       [bridge.Channels]float64
	tick                 int

	btnStart, btnStop *widget.Button
	badge             *stateBadge
	api               *widget.Select
	in, out           *srtForm
	lowLatency        *widget.Check
	rawLatency        *widget.Check
	inDevice          *widget.Select
	send              *widget.Check
	sendFrom          *widget.RadioGroup
	codec             *widget.Select
	bitrate           *widget.SelectEntry
	keepalive         *widget.Check
	monitor           *widget.Check
	monitorFrom       *widget.RadioGroup
	outDevice         *widget.Select
	volume            *widget.Slider
	volumeLabel       *widget.Label
	mute              *widget.Check
	buffer            *widget.SelectEntry
	ffmpeg            *widget.Entry
	ffInfo            *widget.Label
	savePass          *widget.Check
	lightSRT          *statusLight
	lightInput        *statusLight
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
	silent := [bridge.Channels]float64{-90, -90}
	u := &ui{app: a, cfgPath: cfgPath, quit: make(chan struct{}), dbSRT: silent, dbInput: silent}
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

	// Inputs on the left, outputs on the right; each output picks its input.
	// SRT input
	u.in = newSrtForm()
	u.lowLatency = widget.NewCheck("Low-latency start", nil)
	u.rawLatency = widget.NewCheck("Raw low latency (probesize 32)", func(bool) { u.updateEnabled() })
	u.meterSRT = newLevelMeter()
	u.lightSRT = newStatusLight()
	srtIn := newPanel("SRT input", u.lightSRT, nil, container.NewVBox(u.in.box,
		container.NewHBox(u.lowLatency, u.rawLatency), u.meterSRT))

	// local input
	u.inDevice = widget.NewSelect(nil, nil)
	u.meterInput = newLevelMeter()
	u.lightInput = newStatusLight()
	local := newPanel("Local input", u.lightInput, nil, container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Device"), nil, u.inDevice), u.meterInput))

	// SRT output
	u.out = newSrtForm()
	u.send = widget.NewCheck("Send", func(bool) { u.updateEnabled() })
	u.sendFrom = newRouteChoice(func(string) {})
	u.codec = widget.NewSelect(bridge.Codecs, func(string) { u.updateEnabled() })
	bitrates := make([]string, len(bridge.Bitrates))
	for i, b := range bridge.Bitrates {
		bitrates[i] = strconv.Itoa(b)
	}
	u.bitrate = widget.NewSelectEntry(bitrates)
	u.keepalive = widget.NewCheck("Send silence while the input is down (keeps the link up)", nil)
	enc := container.NewHBox(widget.NewLabel("Codec"), withWidth(u.codec, 170),
		widget.NewLabel("Bitrate"), withWidth(u.bitrate, 82), widget.NewLabel("kbit/s"))
	u.lightSender = newStatusLight()
	srtOut := newPanel("SRT output", u.lightSender, u.send, container.NewVBox(
		container.NewHBox(widget.NewLabel("Sends"), u.sendFrom), u.out.box, enc, u.keepalive))

	// monitor
	u.monitor = widget.NewCheck("On", func(bool) { u.monitorChanged() })
	u.monitorFrom = newRouteChoice(func(string) { u.monitorChanged() })
	u.outDevice = widget.NewSelect(nil, func(string) { u.monitorChanged() })
	u.volume = widget.NewSlider(0, 1.5)
	u.volume.Step = 0.01
	u.volume.OnChanged = func(float64) { u.volumeChanged() }
	u.volumeLabel = widget.NewLabel("100%")
	u.mute = widget.NewCheck("Mute", func(bool) { u.volumeChanged() })
	u.buffer = widget.NewSelectEntry(bufferChoices)
	u.buffer.OnChanged = func(string) { u.bufferChanged() }
	u.lightMonitor = newStatusLight()
	mon := newPanel("Monitor", u.lightMonitor, u.monitor, container.NewVBox(
		container.NewHBox(widget.NewLabel("Plays"), u.monitorFrom),
		container.NewBorder(nil, nil, widget.NewLabel("Output"), nil, u.outDevice),
		container.NewBorder(nil, nil, widget.NewLabel("Volume"),
			container.NewHBox(withWidth(u.volumeLabel, 46), u.mute,
				widget.NewLabel("Buffer"), withWidth(u.buffer, 82), widget.NewLabel("ms")),
			u.volume)))

	// settings
	u.ffmpeg = widget.NewEntry()
	u.ffmpeg.SetPlaceHolder("ffmpeg (next to this program, on PATH, or the bundled one)")
	u.ffInfo = widget.NewLabel("")
	u.savePass = widget.NewCheck("Save passphrases", nil)
	settings := newPanel("Settings", nil, nil, container.NewBorder(nil, nil, widget.NewLabel("ffmpeg"),
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
	logPanel := newPanel("Log", nil, copyLog, u.logScroll)

	content := container.New(vstack{gap: 8},
		toolbar,
		container.New(columns{gap: 8}, srtIn, srtOut),
		container.New(columns{gap: 8}, local, mon),
		settings, logPanel)
	// Scrolls only when the window is smaller than the layout (small or
	// high-DPI laptop screens); otherwise the log takes the spare height.
	return container.NewVScroll(container.New(layout.NewCustomPaddedLayout(10, 12, 12, 12), content))
}

func newRouteChoice(changed func(string)) *widget.RadioGroup {
	r := widget.NewRadioGroup(routeLabels, changed)
	r.Horizontal = true
	r.Required = true
	return r
}

// fill puts the settings into the widgets.
func (u *ui) fill() {
	c := u.cfg
	setRoute(u.monitorFrom, c.MonitorSource)
	setRoute(u.sendFrom, c.SendSource)
	u.in.set(c.SrtIn)
	u.out.set(c.SrtOut)
	u.lowLatency.SetChecked(c.LowLatencyInput)
	u.rawLatency.SetChecked(c.RawLowLatency)
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
		MonitorSource:    routeKey(u.monitorFrom),
		SendSource:       routeKey(u.sendFrom),
		SrtIn:            in,
		LowLatencyInput:  u.lowLatency.Checked,
		RawLowLatency:    u.rawLatency.Checked,
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

// updateEnabled greys out the SRT output settings while sending is off. Both
// inputs stay editable: either can be routed to an output at any time.
func (u *ui) updateEnabled() {
	enable(u.lowLatency, !u.rawLatency.Checked) // raw includes everything low-latency start does
	send := u.send.Checked
	u.out.setEnabled(send)
	enable(u.sendFrom, send)
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
		if path == "" && bridge.HasBundledFFmpeg() {
			setInfo("installing…", widget.MediumImportance)
			var err error
			if path, err = bridge.ResolveFFmpeg(hint, u.log); err != nil {
				setInfo("install failed", widget.DangerImportance)
				u.log(err.Error())
				return
			}
		}
		if path == "" {
			setInfo("not found", widget.DangerImportance)
			u.log("ffmpeg not found. Install it, put it next to this program, or set its path.")
			return
		}
		origin := ""
		if bridge.IsBundledFFmpeg(path) {
			origin = " · bundled"
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
			setInfo(version+" · SRT OK"+origin, widget.SuccessImportance)
		} else {
			setInfo(version+" · no SRT"+origin, widget.DangerImportance)
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
	if err := u.engine.SetMonitor(u.monitor.Checked, routeKey(u.monitorFrom), u.api.Selected,
		deviceValue(u.outDevice.Selected), buffer); err != nil {
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

	var peaks bridge.Peaks
	if u.engine != nil {
		peaks = u.engine.TakePeaks()
	}
	fall := func(db *[bridge.Channels]float64, p [bridge.Channels]float64) {
		for i := range p {
			db[i] = max(20*math.Log10(max(p[i], 1e-5)), db[i]-1.2) // ~24 dB/s fall-back
		}
	}
	fall(&u.dbSRT, peaks.SrtIn)
	fall(&u.dbInput, peaks.Input)
	u.meterSRT.setLevels(u.dbSRT)
	u.meterInput.setLevels(u.dbInput)

	if u.tick++; u.tick%5 == 0 {
		u.updateStatus()
	}
}

func (u *ui) updateStatus() {
	off := bridge.ComponentStatus{State: "off"}
	st := bridge.Status{SrtIn: off, Input: off, Monitor: off, Sender: off}
	if u.engine != nil {
		st = u.engine.Snapshot()
	}
	u.lightSRT.set(st.SrtIn.State, st.SrtIn.Detail)
	u.lightInput.set(st.Input.State, st.Input.Detail)
	u.lightMonitor.set(st.Monitor.State, st.Monitor.Detail)
	u.lightSender.set(st.Sender.State, st.Sender.Detail)
	u.badge.set(overall(u.engine != nil, st))
}

// overall is the badge next to Start/Stop. It follows the inputs in use and
// SRT out; the monitor only counts when it fails (it shows "buffering" now
// and then by design).
func overall(running bool, st bridge.Status) (string, fyne.ThemeColorName) {
	if !running {
		return "STOPPED", theme.ColorNameDisabled
	}
	parts := []bridge.ComponentStatus{st.SrtIn, st.Input, st.Sender}
	for _, c := range append(parts, st.Monitor) {
		if c.State == "error" {
			return "ERROR", theme.ColorNameError
		}
	}
	for _, c := range parts {
		if c.State == "no audio" {
			return "NO AUDIO", theme.ColorNameWarning
		}
	}
	for _, c := range parts {
		if c.State != "off" && !goodStates[c.State] {
			return "CONNECTING", theme.ColorNameWarning
		}
	}
	return "LIVE", theme.ColorNameSuccess
}

func (u *ui) onClose() {
	u.shutdown()
	u.win.Close()
}
