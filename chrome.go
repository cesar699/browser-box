package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func findChrome(cfg *config) (string, error) {
	if cfg.chrome != "" {
		if _, err := os.Stat(cfg.chrome); err == nil {
			return cfg.chrome, nil
		}
		return "", fmt.Errorf("chrome binary not found at %s", cfg.chrome)
	}
	candidates := []string{
		filepath.Join(exeDir(), "chrome-linux64", "chrome"),
		filepath.Join(".", "chrome-linux64", "chrome"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome found: put chrome-linux64/ next to the binary or set BROWSERBOX_CHROME")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

type versionInfo struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// superviseChrome keeps Chrome and its CDP session alive: any crash restarts
// the pair after a short pause, and the profile dir survives restarts.
func superviseChrome(cfg *config, a *app) {
	for {
		err := runChromeOnce(cfg, a)
		if err != nil {
			log.Printf("chrome session ended: %v", err)
		}
		a.setController(nil)
		a.hub.setStatus("restarting")
		time.Sleep(3 * time.Second)
	}
}

func runChromeOnce(cfg *config, a *app) error {
	bin, err := findChrome(cfg)
	if err != nil {
		a.hub.setStatus("no chrome")
		return err
	}
	if err := os.MkdirAll(cfg.profile, 0o700); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	args := []string{
		"--headless=new",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--remote-allow-origins=*",
		"--user-data-dir=" + cfg.profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-dev-shm-usage",
		"--no-zygote", // renderers fork directly; zygote breaks in restricted containers/sandboxes
		"--disable-gpu-sandbox",
		"--hide-scrollbars",
		"--autoplay-policy=no-user-gesture-required", // audio must play without a local click (sound goes to the pulse sink)
		fmt.Sprintf("--window-size=%d,%d", cfg.width, cfg.height),
		"--force-device-scale-factor=1",
		"about:blank",
	}
	if os.Geteuid() == 0 {
		// Chrome refuses to run sandboxed as root; the deploy runs as root.
		args = append(args, "--no-sandbox", "--disable-setuid-sandbox")
	}
	log.Printf("starting chrome: %s (debug port %d, profile %s)", bin, port, cfg.profile)
	cmd := exec.Command(bin, args...)
	// Route Chrome's sound into the audio pipeline's pulse sink.
	cmd.Env = append(os.Environ(), "PULSE_SINK=bb", "XDG_RUNTIME_DIR="+pulseDir())
	cmd.Stdout = os.Stderr // chrome logs go to our stderr, prefixed by journald/docker
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	procDone := make(chan error, 1)
	go func() { procDone <- cmd.Wait() }()

	// Wait for the DevTools endpoint.
	var wsURL string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-procDone:
			return fmt.Errorf("chrome exited early: %v", err)
		default:
		}
		wsURL, err = fetchWSURL(port)
		if err == nil && wsURL != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if wsURL == "" {
		_ = cmd.Process.Kill()
		<-procDone
		return fmt.Errorf("chrome devtools endpoint did not come up")
	}

	cdp, err := dialCDP(wsURL)
	if err != nil {
		_ = cmd.Process.Kill()
		<-procDone
		return err
	}
	ctl, err := newController(cfg, a.hub, cdp)
	if err != nil {
		cdp.close()
		_ = cmd.Process.Kill()
		<-procDone
		return err
	}
	a.setController(ctl)
	a.hub.setStatus("ready")
	log.Printf("chrome session ready")

	select {
	case <-cdp.dead:
		_ = cmd.Process.Kill()
		<-procDone
		return fmt.Errorf("cdp connection closed")
	case err := <-procDone:
		cdp.close()
		return fmt.Errorf("chrome exited: %v", err)
	}
}

func fetchWSURL(port int) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.WebSocketDebuggerURL, nil
}
