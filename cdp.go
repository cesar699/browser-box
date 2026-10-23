package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// ---------- minimal CDP client (flatten session mode) ----------

type envelope struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type cdpConn struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	nextID  int
	pending map[int]chan envelope
	onEvent func(env envelope)
	events  chan envelope
	dead    chan struct{}
	once    sync.Once
}

func dialCDP(url string) (*cdpConn, error) {
	conf, err := websocket.NewConfig(url, "http://localhost/")
	if err != nil {
		return nil, err
	}
	ws, err := websocket.DialConfig(conf)
	if err != nil {
		return nil, err
	}
	c := &cdpConn{ws: ws, pending: make(map[int]chan envelope), events: make(chan envelope, 256), dead: make(chan struct{})}
	go c.readLoop()
	go c.eventLoop()
	return c, nil
}

func (c *cdpConn) close() { c.once.Do(func() { close(c.dead); _ = c.ws.Close() }) }

// readLoop only moves bytes: responses to their waiters, events to a queue.
// It must never block on event handling — a handler that itself sends a
// command and waits (e.g. screencast ack) would deadlock the loop.
func (c *cdpConn) readLoop() {
	defer c.close()
	for {
		var raw string
		if err := websocket.Message.Receive(c.ws, &raw); err != nil {
			return
		}
		var env envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			continue
		}
		if env.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[env.ID]
			if ok {
				delete(c.pending, env.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- env
			}
			continue
		}
		select {
		case c.events <- env:
		default: // queue full: drop the event rather than stall responses
		}
	}
}

func (c *cdpConn) eventLoop() {
	for {
		select {
		case <-c.dead:
			return
		case env := <-c.events:
			if c.onEvent != nil {
				c.onEvent(env)
			}
		}
	}
}

// notify sends a command without waiting for its response (fire-and-forget).
func (c *cdpConn) notify(sessionID, method string, params any) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	msg := map[string]any{"id": id, "method": method}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	_ = websocket.Message.Send(c.ws, string(data))
	c.mu.Unlock()
}

func (c *cdpConn) call(sessionID, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan envelope, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	c.mu.Lock()
	err := websocket.Message.Send(c.ws, string(data))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case env := <-ch:
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, env.Error.Message)
		}
		return env.Result, nil
	case <-c.dead:
		return nil, fmt.Errorf("%s: connection closed", method)
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

// ---------- controller: one browser tab, streamed ----------

type controller struct {
	cfg       *config
	hub       *hub
	cdp       *cdpConn
	sessionID string
	mu        sync.Mutex // serialize page commands
	width     int
	height    int
}

type attachResult struct {
	SessionID string `json:"sessionId"`
}
type createTargetResult struct {
	TargetID string `json:"targetId"`
}

func newController(cfg *config, hub *hub, cdp *cdpConn) (*controller, error) {
	ctl := &controller{cfg: cfg, hub: hub, cdp: cdp, width: cfg.width, height: cfg.height}
	cdp.onEvent = ctl.onEvent

	res, err := cdp.call("", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		return nil, err
	}
	var ct createTargetResult
	if err := json.Unmarshal(res, &ct); err != nil {
		return nil, err
	}
	res, err = cdp.call("", "Target.attachToTarget", map[string]any{"targetId": ct.TargetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var at attachResult
	if err := json.Unmarshal(res, &at); err != nil {
		return nil, err
	}
	ctl.sessionID = at.SessionID

	if _, err := ctl.call("Page.enable", nil); err != nil {
		return nil, err
	}
	if err := ctl.setViewport(cfg.width, cfg.height); err != nil {
		return nil, err
	}
	// Keep popups in the same tab: a remote screen has nowhere to show a second window.
	_, _ = ctl.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": "window.open = function(u){ if(u){ location.href = u; } };",
	})
	start := cfg.startURL
	if start == "" {
		start = localStartURL(cfg.listen)
	}
	if start != "" && start != "about:blank" {
		_, _ = ctl.call("Page.navigate", map[string]any{"url": start})
	}
	// The renderer needs a beat to become an "active page" after attach.
	var lastErr error
	for i := 0; i < 10; i++ {
		if lastErr = ctl.startScreencast(); lastErr == nil {
			return ctl, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, lastErr
}

func (ctl *controller) call(method string, params any) (json.RawMessage, error) {
	return ctl.cdp.call(ctl.sessionID, method, params)
}

func (ctl *controller) setViewport(w, h int) error {
	ctl.width, ctl.height = w, h
	_, err := ctl.call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false,
	})
	return err
}

func (ctl *controller) startScreencast() error {
	_, err := ctl.call("Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": 60,
		"maxWidth": ctl.width, "maxHeight": ctl.height,
		"everyNthFrame": 1,
	})
	return err
}

// localStartURL builds the loopback address of the built-in start page from
// the listen address (":8080" → "http://127.0.0.1:8080/start").
func localStartURL(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port + "/start"
}

type screencastFrame struct {
	Data      string `json:"data"`
	SessionID int    `json:"sessionId"`
	Metadata  struct {
		DeviceWidth  float64 `json:"deviceWidth"`
		DeviceHeight float64 `json:"deviceHeight"`
	} `json:"metadata"`
}

func (ctl *controller) onEvent(env envelope) {
	if env.Method != "Page.screencastFrame" || env.SessionID != ctl.sessionID {
		return
	}
	var f screencastFrame
	if err := json.Unmarshal(env.Params, &f); err != nil {
		return
	}
	// Ack first so Chrome keeps sending, then publish. Fire-and-forget:
	// waiting for the ack response here stalled the whole session once.
	ctl.cdp.notify(ctl.sessionID, "Page.screencastFrameAck", map[string]any{"sessionId": f.SessionID})
	jpg, err := base64.StdEncoding.DecodeString(f.Data)
	if err == nil && len(jpg) > 0 {
		ctl.hub.publish(jpg)
	}
}

// ---------- client input ----------

func (ctl *controller) handleClientMsg(m clientMsg) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	switch m.Type {
	case "mousedown", "mouseup", "mousemove":
		typ := map[string]string{
			"mousedown": "mousePressed", "mouseup": "mouseReleased", "mousemove": "mouseMoved",
		}[m.Type]
		p := map[string]any{
			"type": typ, "x": m.X * float64(ctl.width), "y": m.Y * float64(ctl.height),
			"buttons": m.Buttons, "clickCount": m.Clicks,
		}
		if m.Type != "mousemove" {
			p["button"] = m.Button
		} else if m.Button != "" {
			p["button"] = m.Button
		}
		_, _ = ctl.call("Input.dispatchMouseEvent", p)
	case "wheel":
		_, _ = ctl.call("Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": m.X * float64(ctl.width), "y": m.Y * float64(ctl.height),
			"deltaX": m.DeltaX, "deltaY": m.DeltaY,
		})
	case "keydown", "keyup":
		ctl.key(m, m.Type == "keydown")
	case "text":
		_, _ = ctl.call("Input.insertText", map[string]any{"text": m.Text})
	case "navigate":
		if m.URL != "" {
			_, _ = ctl.call("Page.navigate", map[string]any{"url": m.URL})
		}
	case "back":
		_, _ = ctl.call("Runtime.evaluate", map[string]any{"expression": "history.back()"})
	case "forward":
		_, _ = ctl.call("Runtime.evaluate", map[string]any{"expression": "history.forward()"})
	case "reload":
		_, _ = ctl.call("Page.reload", nil)
	case "resize":
		if m.Width >= 320 && m.Height >= 240 && (m.Width != ctl.width || m.Height != ctl.height) {
			_, _ = ctl.call("Page.stopScreencast", nil)
			if err := ctl.setViewport(m.Width, m.Height); err == nil {
				_ = ctl.startScreencast()
			}
		}
	}
}

func (ctl *controller) key(m clientMsg, down bool) {
	// Printable character without command modifiers: a "char" event types it
	// exactly, whatever the viewer's keyboard layout is.
	if down && m.Text != "" && m.Modifiers&0xA == 0 { // no Ctrl(2)/Meta(8)
		_, _ = ctl.call("Input.dispatchKeyEvent", map[string]any{
			"type": "char", "text": m.Text, "unmodifiedText": m.Text,
			"key": m.Key, "modifiers": m.Modifiers,
		})
		return
	}
	typ := "keyUp"
	if down {
		typ = "rawKeyDown"
	}
	p := map[string]any{
		"type": typ, "key": m.Key, "code": m.Code,
		"windowsVirtualKeyCode": m.VK, "nativeVirtualKeyCode": m.VK,
		"modifiers": m.Modifiers,
	}
	// Enter needs a text payload for form submission in some pages.
	if down && m.Key == "Enter" {
		p["text"] = "\r"
		p["unmodifiedText"] = "\r"
	}
	_, _ = ctl.call("Input.dispatchKeyEvent", p)
}

var _ = log.Printf
