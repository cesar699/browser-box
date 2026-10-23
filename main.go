// browser-box: self-hosted remote browser.
// A single Go binary that drives a real Chrome (Chrome for Testing) over CDP,
// streams its frames to your browser over one WebSocket, and sends your
// mouse/keyboard back. One TCP port, password-only login, no Docker, no UDP,
// no NAT config — the whole Neko-era pain list designed out.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

//go:embed web/index.html
var indexHTML []byte

// Start page shown inside the remote Chrome on first launch: proof of life
// that needs no internet, plus quick links.
const startPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>browser-box</title><style>
body{font-family:system-ui,sans-serif;background:#0b0e13;color:#e6ebf2;display:grid;place-items:center;height:100vh;margin:0}
.c{text-align:center} h1{font-size:42px;margin:0 0 8px} p{color:#93a1b5}
a{display:inline-block;margin:8px;padding:12px 22px;background:#1c2330;border:1px solid #2c3648;border-radius:10px;color:#e6ebf2;text-decoration:none;font-size:18px}
</style></head><body><div class="c"><h1>🌐 browser-box</h1>
<p>远程浏览器已就绪。在外层页面顶部的地址栏输入网址即可。</p>
<div><a href="https://www.google.com">Google</a><a href="https://www.youtube.com">YouTube</a><a href="https://github.com">GitHub</a></div>
</div></body></html>`

type config struct {
	listen   string
	password string
	chrome   string // chrome binary path ("" = auto-detect)
	profile  string
	startURL string
	width    int
	height   int
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func loadConfig() *config {
	c := &config{}
	flag.StringVar(&c.listen, "listen", envOr("BROWSERBOX_LISTEN", ":8080"), "listen address")
	flag.StringVar(&c.password, "password", os.Getenv("BROWSERBOX_PASSWORD"), "login password (env BROWSERBOX_PASSWORD)")
	flag.StringVar(&c.chrome, "chrome", os.Getenv("BROWSERBOX_CHROME"), "chrome binary path")
	flag.StringVar(&c.profile, "profile", envOr("BROWSERBOX_PROFILE", filepath.Join(exeDir(), "profile")), "chrome profile dir")
	flag.StringVar(&c.startURL, "url", "", "initial page (empty = built-in start page)")
	flag.IntVar(&c.width, "width", 1280, "initial viewport width")
	flag.IntVar(&c.height, "height", 720, "initial viewport height")
	flag.Parse()
	if c.password == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		c.password = hex.EncodeToString(b)
		log.Printf("no password configured; generated one for this run: %s", c.password)
		log.Printf("set BROWSERBOX_PASSWORD (or -password) to choose your own")
	}
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- sessions ----------

type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessions() *sessions { return &sessions{m: make(map[string]time.Time)} }

func (s *sessions) create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[tok] = time.Now().Add(30 * 24 * time.Hour)
	s.mu.Unlock()
	return tok
}

func (s *sessions) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	return ok && time.Now().Before(exp)
}

func (s *sessions) drop(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// ---------- frame hub ----------
// Broadcasts JPEG frames to all connected viewers. Each viewer keeps only the
// newest frame: a slow client drops frames instead of building up lag.

type hub struct {
	mu      sync.Mutex
	clients map[*viewer]struct{}
	latest  []byte
	status  string
}

type viewer struct {
	ch chan []byte
}

func newHub() *hub { return &hub{clients: make(map[*viewer]struct{}), status: "starting"} }

func (h *hub) add() *viewer {
	v := &viewer{ch: make(chan []byte, 1)}
	h.mu.Lock()
	h.clients[v] = struct{}{}
	latest := h.latest
	h.mu.Unlock()
	if latest != nil {
		v.ch <- latest
	}
	return v
}

func (h *hub) remove(v *viewer) {
	h.mu.Lock()
	delete(h.clients, v)
	h.mu.Unlock()
}

func (h *hub) publish(frame []byte) {
	h.mu.Lock()
	h.latest = frame
	for v := range h.clients {
		select {
		case v.ch <- frame:
		default: // viewer busy: replace its queued frame with the newest
			select {
			case <-v.ch:
			default:
			}
			select {
			case v.ch <- frame:
			default:
			}
		}
	}
	h.mu.Unlock()
}

func (h *hub) setStatus(s string) {
	h.mu.Lock()
	h.status = s
	h.mu.Unlock()
}

func (h *hub) getStatus() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// ---------- app ----------

type app struct {
	cfg  *config
	sess *sessions
	hub  *hub
	ctl  *controller // CDP controller; set once chrome is up
	mu   sync.RWMutex
}

func (a *app) controller() *controller {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ctl
}

func (a *app) setController(c *controller) {
	a.mu.Lock()
	a.ctl = c
	a.mu.Unlock()
}

func (a *app) authed(r *http.Request) bool {
	ck, err := r.Cookie("bb_session")
	return err == nil && a.sess.valid(ck.Value)
}

func main() {
	log.SetFlags(log.Ltime)
	cfg := loadConfig()
	a := &app{cfg: cfg, sess: newSessions(), hub: newHub()}

	// Chrome supervisor: keeps Chrome + the CDP session alive forever.
	go superviseChrome(cfg, a)

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(startPageHTML))
	})
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/api/me", a.handleMe)
	mux.Handle("/ws", websocket.Handler(a.handleWS))

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.listen, err)
	}
	log.Printf("browser-box listening on %s", cfg.listen)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

type loginReq struct {
	Password string `json:"password"`
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.cfg.password)) != 1 {
		time.Sleep(400 * time.Millisecond) // slow down guessing
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tok := a.sess.create()
	http.SetCookie(w, &http.Cookie{
		Name: "bb_session", Value: tok, Path: "/",
		MaxAge: 30 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie("bb_session"); err == nil {
		a.sess.drop(ck.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "bb_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if a.authed(r) {
		_, _ = w.Write([]byte(`{"authed":true}`))
	} else {
		_, _ = w.Write([]byte(`{"authed":false}`))
	}
}

// ---------- websocket: frames out, input in ----------

type clientMsg struct {
	Type string `json:"type"`
	// mouse
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	Button    string  `json:"button,omitempty"` // left|middle|right|none
	Buttons   int     `json:"buttons,omitempty"`
	DeltaX    float64 `json:"deltaX,omitempty"`
	DeltaY    float64 `json:"deltaY,omitempty"`
	Clicks    int     `json:"clickCount,omitempty"`
	// keyboard
	Key       string `json:"key,omitempty"`
	Code      string `json:"code,omitempty"`
	VK        int     `json:"vk,omitempty"`
	Text      string `json:"text,omitempty"`
	Modifiers int     `json:"modifiers,omitempty"`
	Down      bool    `json:"down,omitempty"`
	// nav / misc
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

func (a *app) handleWS(ws *websocket.Conn) {
	defer ws.Close()
	if !a.authed(ws.Request()) {
		_ = websocket.Message.Send(ws, `{"type":"error","message":"unauthorized"}`)
		return
	}
	v := a.hub.add()
	defer a.hub.remove(v)

	// status greeting
	_ = websocket.Message.Send(ws, statusJSON(a.hub.getStatus()))

	// writer: frames + periodic status
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range v.ch {
			if err := websocket.Message.Send(ws, f); err != nil {
				return
			}
		}
	}()

	// reader: client control messages
	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			return
		}
		var m clientMsg
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if c := a.controller(); c != nil {
			c.handleClientMsg(m)
		}
	}
}

func statusJSON(s string) string {
	b, _ := json.Marshal(map[string]string{"type": "status", "status": s})
	return string(b)
}
