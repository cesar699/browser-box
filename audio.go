package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Audio path: Chrome plays into a PulseAudio null sink ("bb"); ffmpeg
// captures the sink's monitor and encodes AAC in fragmented MP4, which the
// server fans out to any viewer that turned sound on. Best-effort: if any
// piece is missing, the browser keeps working silently.

func pulseDir() string { return filepath.Join(exeDir(), ".pulse") }

func pulseEnv() []string {
	return append(os.Environ(), "XDG_RUNTIME_DIR="+pulseDir())
}

func pactl(args ...string) error {
	cmd := exec.Command("pactl", args...)
	cmd.Env = pulseEnv()
	return cmd.Run()
}

func ensurePulse() bool {
	if _, err := exec.LookPath("pulseaudio"); err != nil {
		return false
	}
	if err := os.MkdirAll(pulseDir(), 0o700); err != nil {
		return false
	}
	if pactl("info") != nil {
		cmd := exec.Command("pulseaudio", "--daemonize=yes", "--exit-idle-time=-1")
		cmd.Env = pulseEnv()
		if err := cmd.Run(); err != nil {
			log.Printf("audio: pulseaudio start: %v", err)
			return false
		}
		for i := 0; i < 15 && pactl("info") != nil; i++ {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if pactl("info") != nil {
		return false
	}
	found := false
	cmd := exec.Command("pactl", "list", "sinks", "short")
	cmd.Env = pulseEnv()
	if b, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "\tbb\t") {
				found = true
			}
		}
	}
	if !found {
		if err := pactl("load-module", "module-null-sink", "sink_name=bb", "sink_properties=device.description=BrowserBox"); err != nil {
			log.Printf("audio: load null sink: %v", err)
			return false
		}
	}
	_ = pactl("set-default-sink", "bb")
	return true
}

// runAudioLoop keeps an ffmpeg capture of the bb monitor alive forever,
// broadcasting fMP4 chunks to the hub. Restarts on failure.
func runAudioLoop(h *hub) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("audio: ffmpeg not found, audio disabled")
		return
	}
	for {
		if !ensurePulse() {
			log.Printf("audio: pulse unavailable, retrying in 10s")
			time.Sleep(10 * time.Second)
			continue
		}
		if err := captureOnce(h); err != nil {
			log.Printf("audio: capture ended: %v (restarting)", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func captureOnce(h *hub) error {
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "pulse", "-i", "bb.monitor",
		"-vn", "-ac", "2", "-ar", "48000",
		"-c:a", "aac", "-b:a", "96k",
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof+omit_tfhd_offset",
		"-frag_duration", "250000",
		"pipe:1",
	)
	cmd.Env = pulseEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Wait() }()

	buf := make([]byte, 16384)
	var pending []byte
	initDone := false
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !initDone {
				pending = append(pending, chunk...)
				if idx := bytes.Index(pending, []byte("moof")); idx >= 4 {
					init := append([]byte(nil), pending[:idx-4]...)
					h.setAudioInit(init)
					rest := append([]byte(nil), pending[idx-4:]...)
					h.publishAudio(rest)
					pending = nil
					initDone = true
					log.Printf("audio: streaming (init segment %d bytes)", len(init))
				} else if len(pending) > 1<<20 {
					// No moof in sight: give up on init caching, stream raw.
					h.publishAudio(append([]byte(nil), pending...))
					pending = nil
					initDone = true
				}
			} else {
				h.publishAudio(append([]byte(nil), chunk...))
			}
		}
		if rerr != nil {
			_ = cmd.Process.Kill()
			return rerr
		}
	}
}
