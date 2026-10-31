//go:build !windows

package bridge

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFFmpeg is a shell script that answers -protocols with or without srt.
func fakeFFmpeg(withSRT bool) []byte {
	proto := "file"
	if withSRT {
		proto = "srt"
	}
	return []byte("#!/bin/sh\necho 'Supported file protocols:'\necho 'Input:'\necho '  " + proto + "'\n")
}

// withBundle makes this test binary carry payload as its bundled ffmpeg,
// unpacked under a temporary directory.
func withBundle(t *testing.T, payload []byte) string {
	t.Helper()
	saved, savedRoot := bundle, bundleRoot
	t.Cleanup(func() { bundle, bundleRoot = saved, savedRoot })
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(payload)
	zw.Close()
	sum := sha256.Sum256(payload)
	bundle.gz = gz.Bytes()
	bundle.sha256 = hex.EncodeToString(sum[:])
	bundle.about = "test build"
	bundle.extras = map[string][]byte{"LICENSE.txt": []byte("license")}
	root := t.TempDir()
	bundleRoot = func() (string, error) { return root, nil }
	return root
}

// pathWith puts a fake ffmpeg (or nothing) on PATH.
func pathWith(t *testing.T, ffmpeg []byte) string {
	dir := t.TempDir()
	if ffmpeg != nil {
		if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), ffmpeg, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return filepath.Join(dir, "ffmpeg")
}

func TestInstallBundledFFmpeg(t *testing.T) {
	root := withBundle(t, fakeFFmpeg(true))
	stale := filepath.Join(root, "0123456789ab")
	os.MkdirAll(stale, 0o755)

	var logs []string
	path, err := InstallBundledFFmpeg(func(s string) { logs = append(logs, s) })
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("not an executable file: %v %v", st, err)
	}
	if !hasSRT(path) || !IsBundledFFmpeg(path) {
		t.Error("unpacked ffmpeg doesn't run or isn't recognised as bundled")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "LICENSE.txt")); err != nil {
		t.Error("license not written next to ffmpeg")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("version from an earlier release not removed")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "Installing the bundled ffmpeg (test build)") {
		t.Errorf("logs %q", logs)
	}
	again, err := InstallBundledFFmpeg(func(s string) { t.Errorf("installed twice: %s", s) })
	if err != nil || again != path {
		t.Errorf("second call: %s %v", again, err)
	}
}

func TestInstallRejectsDamagedBundle(t *testing.T) {
	withBundle(t, fakeFFmpeg(true))
	bundle.sha256 = strings.Repeat("0", 64)
	if _, err := InstallBundledFFmpeg(func(string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("got %v", err)
	}
	if installedBundle() != "" {
		t.Error("damaged copy marked as installed")
	}
}

func TestResolveFFmpegOrder(t *testing.T) {
	nop := func(string) {}

	t.Run("installed ffmpeg with SRT wins", func(t *testing.T) {
		withBundle(t, fakeFFmpeg(true))
		onPath := pathWith(t, fakeFFmpeg(true))
		if got, err := ResolveFFmpeg("ffmpeg", nop); err != nil || got != onPath {
			t.Errorf("got %s %v, want %s", got, err, onPath)
		}
	})
	t.Run("installed ffmpeg without SRT is skipped", func(t *testing.T) {
		withBundle(t, fakeFFmpeg(true))
		pathWith(t, fakeFFmpeg(false))
		if got, err := ResolveFFmpeg("ffmpeg", nop); err != nil || !IsBundledFFmpeg(got) {
			t.Errorf("got %s %v, want the bundled one", got, err)
		}
	})
	t.Run("nothing installed", func(t *testing.T) {
		withBundle(t, fakeFFmpeg(true))
		pathWith(t, nil)
		if got, err := ResolveFFmpeg("", nop); err != nil || !IsBundledFFmpeg(got) {
			t.Errorf("got %s %v, want the bundled one", got, err)
		}
		if FindFFmpeg("ffmpeg") == "" {
			t.Error("FindFFmpeg should see the unpacked bundle afterwards")
		}
	})
	t.Run("configured path is gone", func(t *testing.T) {
		withBundle(t, fakeFFmpeg(true))
		pathWith(t, nil)
		var logs []string
		got, err := ResolveFFmpeg("/old/place/ffmpeg", func(s string) { logs = append(logs, s) })
		if err != nil || !IsBundledFFmpeg(got) || !strings.Contains(strings.Join(logs, "\n"), "not found at /old/place/ffmpeg") {
			t.Errorf("got %s %v %q", got, err, logs)
		}
	})
	t.Run("no bundle, nothing installed", func(t *testing.T) {
		saved := bundle
		t.Cleanup(func() { bundle = saved })
		bundle.gz = nil
		pathWith(t, nil)
		if _, err := ResolveFFmpeg("ffmpeg", nop); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("got %v", err)
		}
	})
}
