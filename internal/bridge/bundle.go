package bridge

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Release builds carry a gzip-compressed ffmpeg (see bundle_embed.go, built
// with -tags bundleffmpeg). When no usable ffmpeg is installed, it is unpacked
// once into the user's cache directory (%LOCALAPPDATA% on Windows) and run
// from there. Development builds have no bundle and behave as before.
var bundle struct {
	gz     []byte            // compressed ffmpeg binary
	sha256 string            // hex SHA-256 of the uncompressed binary
	about  string            // one line naming the build, for the log
	extras map[string][]byte // written next to it (license, source note)
}

// bundleRoot is where bundled ffmpeg versions are unpacked; tests override it.
var bundleRoot = func() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "SRT Audio Bridge", "ffmpeg"), nil
}

var bundleMu sync.Mutex

// HasBundledFFmpeg reports whether this build carries its own ffmpeg.
func HasBundledFFmpeg() bool { return len(bundle.gz) > 0 && len(bundle.sha256) >= 12 }

func ffmpegExeName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func bundleDir() (string, error) {
	root, err := bundleRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, bundle.sha256[:12]), nil
}

// installedBundle returns the unpacked bundled ffmpeg, or "" if it isn't
// (completely) there. The marker file is written last, after a verified copy.
func installedBundle() string {
	if !HasBundledFFmpeg() {
		return ""
	}
	dir, err := bundleDir()
	if err != nil {
		return ""
	}
	marker, err := os.ReadFile(filepath.Join(dir, "installed.sha256"))
	if err != nil || strings.TrimSpace(string(marker)) != bundle.sha256 {
		return ""
	}
	path := filepath.Join(dir, ffmpegExeName())
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return ""
	}
	return path
}

// IsBundledFFmpeg reports whether path is the unpacked bundled ffmpeg.
func IsBundledFFmpeg(path string) bool {
	p := installedBundle()
	return p != "" && filepath.Clean(p) == filepath.Clean(path)
}

// InstallBundledFFmpeg unpacks the bundled ffmpeg if needed and returns its path.
func InstallBundledFFmpeg(log func(string)) (string, error) {
	if !HasBundledFFmpeg() {
		return "", errors.New("this build has no bundled ffmpeg")
	}
	bundleMu.Lock()
	defer bundleMu.Unlock()
	if p := installedBundle(); p != "" {
		return p, nil
	}
	dir, err := bundleDir()
	if err != nil {
		return "", err
	}
	log("Installing the bundled ffmpeg (" + bundle.about + ") to " + dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "ffmpeg-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	zr, err := gzip.NewReader(bytes.NewReader(bundle.gz))
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("bundled ffmpeg is damaged: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), zr)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("unpacking ffmpeg: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != bundle.sha256 {
		return "", errors.New("bundled ffmpeg is damaged (checksum mismatch)")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ffmpegExeName())
	if err := os.Rename(tmp.Name(), path); err != nil {
		// Another instance may have finished first and be running it.
		if _, statErr := os.Stat(path); statErr != nil {
			return "", err
		}
	}
	for name, data := range bundle.extras {
		_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	if err := os.WriteFile(filepath.Join(dir, "installed.sha256"), []byte(bundle.sha256+"\n"), 0o644); err != nil {
		return "", err
	}
	removeOldBundles(dir)
	return path, nil
}

// removeOldBundles deletes versions unpacked by earlier releases. One still in
// use by another running instance can't be deleted on Windows; that is fine.
func removeOldBundles(keep string) {
	root := filepath.Dir(keep)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if p := filepath.Join(root, e.Name()); e.IsDir() && p != keep {
			_ = os.RemoveAll(p)
		}
	}
}

var srtSupport sync.Map // ffmpeg path -> bool

// hasSRT reports whether an ffmpeg binary lists the srt protocol (cached).
func hasSRT(path string) bool {
	if v, ok := srtSupport.Load(path); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-protocols")
	hideWindow(cmd)
	out, err := cmd.Output()
	ok := err == nil && srtProtoRE.Match(out)
	srtSupport.Store(path, ok)
	return ok
}

// ResolveFFmpeg finds ffmpeg like FindFFmpeg and, when there is none (or the
// configured file is gone), installs the bundled one.
func ResolveFFmpeg(hint string, log func(string)) (string, error) {
	if p := FindFFmpeg(hint); p != "" {
		return p, nil
	}
	if !HasBundledFFmpeg() {
		return "", errors.New("ffmpeg not found. Install it or set its path.")
	}
	if h := strings.TrimSpace(hint); h != "" && h != "ffmpeg" {
		log("ffmpeg not found at " + h + "; using the bundled one")
	}
	p, err := InstallBundledFFmpeg(log)
	if err != nil {
		return "", fmt.Errorf("can't install the bundled ffmpeg: %w", err)
	}
	return p, nil
}
