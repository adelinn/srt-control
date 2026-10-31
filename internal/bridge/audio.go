package bridge

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/gen2brain/malgo"
)

// Audio devices go through miniaudio (malgo). It is compiled into the binary,
// so there is no DLL to ship, and it converts sample rate, format and channel
// count, so every device looks like 48 kHz s16 stereo to the rest of the code.

type backendDef struct {
	id   malgo.Backend
	name string
}

// Preferred first: the first one that works is the default.
func candidateBackends() []backendDef {
	switch runtime.GOOS {
	case "windows":
		return []backendDef{{malgo.BackendWasapi, "WASAPI"}, {malgo.BackendDsound, "DirectSound"}, {malgo.BackendWinmm, "MME"}}
	case "darwin":
		return []backendDef{{malgo.BackendCoreaudio, "Core Audio"}}
	default:
		return []backendDef{{malgo.BackendPulseaudio, "PulseAudio"}, {malgo.BackendAlsa, "ALSA"}}
	}
}

var audioSys struct {
	once  sync.Once
	names []string
	ctx   map[string]*malgo.AllocatedContext
}

func initAudio() {
	audioSys.once.Do(func() {
		audioSys.ctx = map[string]*malgo.AllocatedContext{}
		for _, b := range candidateBackends() {
			c, err := malgo.InitContext([]malgo.Backend{b.id}, malgo.ContextConfig{}, nil)
			if err != nil {
				continue
			}
			audioSys.ctx[b.name] = c
			audioSys.names = append(audioSys.names, b.name)
		}
	})
}

// AudioBackends lists the audio APIs that work on this machine, preferred first.
func AudioBackends() []string {
	initAudio()
	return append([]string(nil), audioSys.names...)
}

// resolveBackend falls back to the preferred backend when name is empty or unknown.
func resolveBackend(name string) (string, *malgo.AllocatedContext, error) {
	initAudio()
	if c, ok := audioSys.ctx[name]; ok {
		return name, c, nil
	}
	if len(audioSys.names) == 0 {
		return "", nil, errors.New("no audio system available")
	}
	n := audioSys.names[0]
	return n, audioSys.ctx[n], nil
}

// AudioDevice is one input or output device of a backend.
type AudioDevice struct {
	Name    string
	Default bool
	id      malgo.DeviceID
}

// ListDevices enumerates devices afresh on every call, so newly plugged
// devices show up without restarting anything.
func ListDevices(backend string, capture bool) ([]AudioDevice, error) {
	_, c, err := resolveBackend(backend)
	if err != nil {
		return nil, err
	}
	kind := malgo.Playback
	if capture {
		kind = malgo.Capture
	}
	infos, err := c.Devices(kind)
	if err != nil {
		return nil, err
	}
	out := make([]AudioDevice, 0, len(infos))
	seen := map[string]int{}
	for _, d := range infos {
		name := d.Name()
		if seen[name]++; seen[name] > 1 { // two identical interfaces: keep the names distinct
			name = fmt.Sprintf("%s #%d", name, seen[name])
		}
		out = append(out, AudioDevice{Name: name, Default: d.IsDefault != 0, id: d.ID})
	}
	return out, nil
}

// matchDevice finds a saved device name. Labels saved by the Python version
// looked like "Name  [API]", so that suffix is ignored.
func matchDevice(devs []AudioDevice, name string) *AudioDevice {
	for _, try := range []string{name, strings.SplitN(name, "  [", 2)[0]} {
		for i := range devs {
			if devs[i].Name == try {
				return &devs[i]
			}
		}
	}
	return nil
}

// openDevice opens a 48 kHz s16 stereo stream on a device ("" = system
// default, which also follows changes of the default device). It returns the
// device and a label for the log and status line.
func openDevice(backend, name string, capture bool, cb malgo.DeviceCallbacks) (*malgo.Device, string, error) {
	bname, c, err := resolveBackend(backend)
	if err != nil {
		return nil, "", err
	}
	kind, what := malgo.Playback, "output"
	if capture {
		kind, what = malgo.Capture, "input"
	}
	cfg := malgo.DefaultDeviceConfig(kind)
	cfg.SampleRate = Rate
	cfg.PeriodSizeInFrames = BlockFrames
	sub := &cfg.Playback
	if capture {
		sub = &cfg.Capture
	}
	sub.Format = malgo.FormatS16
	sub.Channels = Channels

	devs, err := ListDevices(bname, capture)
	if err != nil {
		return nil, "", err
	}
	label := "System default"
	var pin runtime.Pinner // the config handed to C points at the device ID
	defer pin.Unpin()
	if name != "" {
		d := matchDevice(devs, name)
		if d == nil {
			return nil, "", fmt.Errorf("%s device not found: %s", what, name)
		}
		id := new(malgo.DeviceID)
		*id = d.id
		pin.Pin(id)
		sub.DeviceID = id.Pointer()
		label = d.Name
	} else {
		for _, d := range devs {
			if d.Default {
				label = d.Name
				break
			}
		}
	}
	dev, err := malgo.InitDevice(c.Context, cfg, cb)
	if err != nil {
		return nil, "", fmt.Errorf("can't open %s device %s: %w", what, label, err)
	}
	return dev, fmt.Sprintf("%s  [%s]", label, bname), nil
}
