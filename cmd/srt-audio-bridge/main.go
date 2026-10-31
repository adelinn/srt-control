// SRT Audio Bridge: receive audio over SRT (or from a local input device),
// monitor it on any output device, and forward it to another SRT destination.
//
//	srt-audio-bridge                    GUI
//	srt-audio-bridge -list-devices      print audio devices and exit
//	srt-audio-bridge -headless          run the saved settings without a GUI
//	srt-audio-bridge -config FILE       use another settings file (presets)
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adelinn/srt-control/internal/bridge"
	"github.com/adelinn/srt-control/internal/gui"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		attachConsole()
	}
	configPath := flag.String("config", bridge.DefaultConfigPath(), "settings file")
	headless := flag.Bool("headless", false, "run the saved settings without a GUI")
	listDevices := flag.Bool("list-devices", false, "print audio devices and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("SRT Audio Bridge", version)
	case *listDevices:
		printDevices()
	case *headless:
		os.Exit(runHeadless(*configPath))
	default:
		gui.Run(*configPath)
	}
}

func printDevices() {
	backends := bridge.AudioBackends()
	if len(backends) == 0 {
		fmt.Println("No audio system available.")
		return
	}
	for _, b := range backends {
		fmt.Println(b)
		for _, kind := range []struct {
			name    string
			capture bool
		}{{"in ", true}, {"out", false}} {
			devs, err := bridge.ListDevices(b, kind.capture)
			if err != nil {
				fmt.Printf("  %s  error: %v\n", kind.name, err)
			}
			for _, d := range devs {
				mark := ""
				if d.Default {
					mark = "  (default)"
				}
				fmt.Printf("  %s  %s%s\n", kind.name, d.Name, mark)
			}
		}
	}
}

func runHeadless(configPath string) int {
	if _, err := os.Stat(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "No settings file at %s. Start the GUI once and press Start, "+
			"or pass -config with a saved file.\n", configPath)
		return 1
	}
	cfg, err := bridge.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var mu sync.Mutex
	log := func(msg string) { // one write per line so goroutines don't interleave
		mu.Lock()
		fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), bridge.MaskSecrets(msg))
		mu.Unlock()
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	engine := bridge.NewEngine(cfg, log)
	if err := engine.Start(); err != nil {
		log("Start failed: " + err.Error())
		return 1
	}
	log("Running from " + configPath + ". Ctrl+C to stop.")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for n := 1; ; n++ {
		select {
		case <-stop:
			engine.Stop()
			log("Stopped")
			return 0
		case <-tick.C:
		}
		st := engine.Snapshot()
		var parts []string
		for _, c := range []struct {
			name string
			s    bridge.ComponentStatus
		}{{"srt in", st.SrtIn}, {"input", st.Input}, {"monitor", st.Monitor}, {"sender", st.Sender}} {
			p := c.name + ": " + c.s.State
			if c.s.Detail != "" {
				p += " (" + c.s.Detail + ")"
			}
			parts = append(parts, p)
		}
		if line := strings.Join(parts, " | "); line != last {
			log(line)
			last = line
		}
		peaks := engine.TakePeaks()
		if n%30 == 0 {
			db := func(p [bridge.Channels]float64) string {
				return fmt.Sprintf("%.1f / %.1f dB", 20*math.Log10(max(p[0], 1e-5)), 20*math.Log10(max(p[1], 1e-5)))
			}
			var levels []string
			if st.SrtIn.State != "off" {
				levels = append(levels, "srt in "+db(peaks.SrtIn))
			}
			if st.Input.State != "off" {
				levels = append(levels, "input "+db(peaks.Input))
			}
			log("levels: " + strings.Join(levels, " | "))
		}
	}
}
