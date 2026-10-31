package bridge

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// FindFFmpeg resolves the ffmpeg binary: an explicit file path first, then an
// ffmpeg next to this program (handy for a bundled Windows build), then PATH.
func FindFFmpeg(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		hint = "ffmpeg"
	}
	if st, err := os.Stat(hint); err == nil && !st.IsDir() && strings.ContainsAny(hint, `/\`) {
		return hint
	}
	if hint == "ffmpeg" {
		if exe, err := os.Executable(); err == nil {
			local := filepath.Join(filepath.Dir(exe), "ffmpeg")
			if runtime.GOOS == "windows" {
				local += ".exe"
			}
			if st, err := os.Stat(local); err == nil && !st.IsDir() {
				return local
			}
		}
	}
	if p, err := exec.LookPath(hint); err == nil {
		return p
	}
	return ""
}

// FFmpegInfo describes an ffmpeg binary.
type FFmpegInfo struct {
	Version  string
	SRT      bool
	Encoders map[string]bool
}

var (
	versionRE  = regexp.MustCompile(`version (\S+)`)
	srtProtoRE = regexp.MustCompile(`(?m)^\s*srt\s*$`)
	encoderRE  = regexp.MustCompile(`(?m)^\s*A[.\w]{5}\s+(\S+)`)
)

// ProbeFFmpeg reports the version, SRT support and audio encoders of a binary.
func ProbeFFmpeg(path string) (FFmpegInfo, error) {
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, append([]string{"-hide_banner"}, args...)...)
		hideWindow(cmd)
		out, err := cmd.Output()
		return string(out), err
	}
	version, err := run("-version")
	if err != nil {
		return FFmpegInfo{}, err
	}
	protocols, err := run("-protocols")
	if err != nil {
		return FFmpegInfo{}, err
	}
	encoders, err := run("-encoders")
	if err != nil {
		return FFmpegInfo{}, err
	}
	info := FFmpegInfo{Version: "unknown", Encoders: map[string]bool{}}
	if m := versionRE.FindStringSubmatch(version); m != nil {
		info.Version = m[1]
	}
	info.SRT = srtProtoRE.MatchString(protocols)
	for _, m := range encoderRE.FindAllStringSubmatch(encoders, -1) {
		info.Encoders[m[1]] = true
	}
	return info, nil
}

// ffproc is one running ffmpeg process.
type ffproc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser // nil unless requested
	stdout io.ReadCloser
	exited chan struct{} // closed once the process has been reaped
}

func startFFmpeg(args []string, withStdin bool, tag string, log func(string), seen *errorDedupe) (*ffproc, error) {
	cmd := exec.Command(args[0], args[1:]...)
	hideWindow(cmd)
	p := &ffproc{cmd: cmd, exited: make(chan struct{})}
	var err error
	if withStdin {
		if p.stdin, err = cmd.StdinPipe(); err != nil {
			return nil, err
		}
	}
	if p.stdout, err = cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	afterStart(cmd.Process)
	go func() {
		pumpStderr(stderr, tag, log, seen) // returns when ffmpeg exits
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func (p *ffproc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// stop ends ffmpeg. Closing stdin first lets an encoder flush and close its SRT
// socket cleanly (the only graceful option on Windows); then SIGTERM, then kill.
func (p *ffproc) stop() {
	if p.stdin != nil {
		_ = p.stdin.Close()
		select {
		case <-p.exited:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	if !p.running() {
		return
	}
	_ = interrupt(p.cmd.Process)
	select {
	case <-p.exited:
		return
	case <-time.After(1500 * time.Millisecond):
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
	}
}

// ffmpeg warnings that are expected here and only add noise to the log
var logNoiseRE = regexp.MustCompile(`Guessed Channel Layout|frame size not set|Estimating duration`)
var addrRE = regexp.MustCompile(` @ 0x[0-9a-f]+`)

// errorDedupe remembers when each ffmpeg message was last shown. While a far
// end is down, ffmpeg repeats the same errors on every retry.
type errorDedupe struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (d *errorDedupe) shouldShow(line string) bool {
	key := addrRE.ReplaceAllString(line, "")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]time.Time{}
	}
	now := time.Now()
	if t, ok := d.last[key]; ok && now.Sub(t) < time.Minute {
		return false
	}
	d.last[key] = now
	return true
}

// pumpStderr forwards ffmpeg's stderr to the log, repeats at most once a minute.
func pumpStderr(r io.Reader, tag string, log func(string), seen *errorDedupe) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || logNoiseRE.MatchString(line) || !seen.shouldShow(line) {
			continue
		}
		log(tag + ": " + line)
	}
}

// cmdline renders a command for the log, quoted for the local shell.
func cmdline(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case a != "" && !strings.ContainsAny(a, " \t\"'&?;|<>()$`*\\"):
			out[i] = a
		case runtime.GOOS == "windows":
			out[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		default:
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
