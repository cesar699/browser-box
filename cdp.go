package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"path/filepath"
	"strings"
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
// command and waits would deadlock the loop (learned the hard way).
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
		default:
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

// ---------- controller: multi-tab browser session ----------

type tab struct {
	id        string // targetId
	sessionID string
	title     string
	url       string
}

type chooserReq struct {
	sessionID string
	nodeID    int
}

type controller struct {
	cfg *config
	hub *hub
	cdp *cdpConn

	mu      sync.Mutex
	tabs    map[string]*tab
	order   []string
	known   map[string]bool // targetIds we created/attached ourselves
	active  string
	width   int
	height  int
	quality int
	dsf     float64
	casting bool
	viewers int

	pendingChooser *chooserReq
	lastUpload     string
	lastUploadAt   time.Time
	downloads      map[string]string // guid -> filename
	saveTimer      *time.Timer
}

func newController(cfg *config, hub *hub, cdp *cdpConn) (*controller, error) {
	ctl := &controller{
		cfg: cfg, hub: hub, cdp: cdp,
		tabs: make(map[string]*tab), known: make(map[string]bool),
		width: cfg.width, height: cfg.height, quality: 60, dsf: 1,
		downloads: make(map[string]string),
	}
	cdp.onEvent = ctl.onEvent

	abs, _ := filepath.Abs(downloadsDir())
	if _, err := cdp.call("", "Browser.setDownloadBehavior", map[string]any{
		"behavior": "allow", "downloadPath": abs, "eventsEnabled": true,
	}); err != nil {
		log.Printf("setDownloadBehavior: %v", err)
	}
	if _, err := cdp.call("", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		log.Printf("setDiscoverTargets: %v", err)
	}
	hub.onViewers = ctl.viewersChanged
	ctl.viewers = hub.viewerCount() // a client may have connected before we existed

	start := cfg.startURL
	if start == "" {
		if last := loadState().LastURL; last != "" {
			start = last
		} else {
			start = localStartURL(cfg.listen)
		}
	}
	// Chrome starts with its own initial tab: adopt it instead of opening a
	// second one (discovery would otherwise show both). Hold ctl.mu for the
	// whole setup: the event loop is already running and must not attach
	// the same target concurrently.
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	adopted := false
	if res, err := cdp.call("", "Target.getTargets", nil); err == nil {
		var gt struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfos"`
		}
		if err := json.Unmarshal(res, &gt); err == nil {
			for _, ti := range gt.TargetInfos {
				if ti.Type == "page" {
					ctl.known[ti.TargetID] = true
					if t, err := ctl.attachLocked(ti.TargetID); err == nil {
						if start != "" && start != "about:blank" {
							_, _ = ctl.tabCall(t, "Page.navigate", map[string]any{"url": start})
						}
						ctl.activateLocked(t.id)
						adopted = true
					}
					break
				}
			}
		}
	}
	if !adopted {
		if _, err := ctl.openTabLocked(start); err != nil {
			return nil, err
		}
	}
	return ctl, nil
}

func (ctl *controller) activeTab() *tab {
	if ctl.active == "" {
		return nil
	}
	return ctl.tabs[ctl.active]
}

func (ctl *controller) call(method string, params any) (json.RawMessage, error) {
	t := ctl.activeTab()
	if t == nil {
		return nil, fmt.Errorf("no active tab")
	}
	return ctl.cdp.call(t.sessionID, method, params)
}

func (ctl *controller) tabCall(t *tab, method string, params any) (json.RawMessage, error) {
	return ctl.cdp.call(t.sessionID, method, params)
}

// openTabLocked creates and attaches a new tab. Caller holds ctl.mu
// (except during construction, when nothing else runs yet).
func (ctl *controller) openTabLocked(rawURL string) (*tab, error) {
	res, err := ctl.cdp.call("", "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		return nil, err
	}
	var ct struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(res, &ct); err != nil {
		return nil, err
	}
	ctl.known[ct.TargetID] = true
	t, err := ctl.attachLocked(ct.TargetID)
	if err != nil {
		return nil, err
	}
	if rawURL != "" && rawURL != "about:blank" {
		_, _ = ctl.tabCall(t, "Page.navigate", map[string]any{"url": rawURL})
	}
	ctl.activateLocked(t.id)
	return t, nil
}

func (ctl *controller) attachLocked(targetID string) (*tab, error) {
	// targetCreated events race with our own openTab: never attach twice.
	if existing := ctl.tabs[targetID]; existing != nil {
		return existing, nil
	}
	res, err := ctl.cdp.call("", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var at struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &at); err != nil {
		return nil, err
	}
	t := &tab{id: targetID, sessionID: at.SessionID}
	ctl.tabs[targetID] = t
	ctl.order = append(ctl.order, targetID)
	if _, err := ctl.tabCall(t, "Page.enable", nil); err != nil {
		return nil, err
	}
	_, _ = ctl.tabCall(t, "Page.setInterceptFileChooserDialog", map[string]any{"enabled": true})
	_, _ = ctl.tabCall(t, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": "window.open = function(u){ if(u){ location.href = u; } };",
	})
	_ = ctl.applyViewport(t)
	return t, nil
}

func (ctl *controller) applyViewport(t *tab) error {
	_, err := ctl.tabCall(t, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": ctl.width, "height": ctl.height, "deviceScaleFactor": ctl.dsf, "mobile": false,
	})
	return err
}

func (ctl *controller) activateLocked(id string) {
	t := ctl.tabs[id]
	if t == nil {
		return
	}
	if ctl.active != "" && ctl.active != id && ctl.casting {
		if old := ctl.tabs[ctl.active]; old != nil {
			_, _ = ctl.tabCall(old, "Page.stopScreencast", nil)
		}
		ctl.casting = false
	}
	ctl.active = id
	if ctl.viewers > 0 && !ctl.casting {
		if err := ctl.startScreencastLocked(); err == nil {
			ctl.casting = true
		}
	}
	ctl.sendTabState()
}

func (ctl *controller) startScreencastLocked() error {
	t := ctl.activeTab()
	if t == nil {
		return fmt.Errorf("no active tab")
	}
	var lastErr error
	for i := 0; i < 10; i++ {
		_, lastErr = ctl.tabCall(t, "Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": ctl.quality,
			"maxWidth": int(float64(ctl.width) * ctl.dsf), "maxHeight": int(float64(ctl.height) * ctl.dsf),
			"everyNthFrame": 1,
		})
		if lastErr == nil {
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return lastErr
}

func (ctl *controller) stopScreencastLocked() {
	t := ctl.activeTab()
	if t != nil && ctl.casting {
		_, _ = ctl.tabCall(t, "Page.stopScreencast", nil)
	}
	ctl.casting = false
}

func (ctl *controller) closeTabLocked(id string) {
	t := ctl.tabs[id]
	if t == nil {
		return
	}
	_, _ = ctl.cdp.call("", "Target.closeTarget", map[string]any{"targetId": id})
	delete(ctl.tabs, id)
	delete(ctl.known, id)
	for i, oid := range ctl.order {
		if oid == id {
			ctl.order = append(ctl.order[:i], ctl.order[i+1:]...)
			break
		}
	}
	if ctl.active == id {
		ctl.active = ""
		ctl.casting = false
		if len(ctl.order) > 0 {
			ctl.activateLocked(ctl.order[len(ctl.order)-1])
		} else {
			// Never leave the user with zero tabs.
			_, _ = ctl.openTabLocked(localStartURL(ctl.cfg.listen))
		}
	}
	ctl.sendTabState()
}

func (ctl *controller) viewersChanged(n int) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	ctl.viewers = n
	if n == 0 && ctl.casting {
		ctl.stopScreencastLocked() // save the VPS CPU when nobody is watching
	}
	if n > 0 && !ctl.casting && ctl.active != "" {
		if err := ctl.startScreencastLocked(); err == nil {
			ctl.casting = true
		}
	}
}

// ---------- tab state to clients ----------

type tabInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// sendTabStateSync is for callers that do not hold ctl.mu (new WS clients).
func (ctl *controller) sendTabStateSync() {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	ctl.sendTabState()
}

// sendTabState requires ctl.mu to be held.
func (ctl *controller) sendTabState() {
	infos := make([]tabInfo, 0, len(ctl.order))
	activeURL, activeTitle := "", ""
	for _, id := range ctl.order {
		t := ctl.tabs[id]
		if t == nil {
			continue
		}
		title := t.title
		if title == "" {
			title = t.url
		}
		infos = append(infos, tabInfo{ID: t.id, Title: title, URL: t.url, Active: t.id == ctl.active})
		if t.id == ctl.active {
			activeURL, activeTitle = t.url, t.title
		}
	}
	b, _ := json.Marshal(map[string]any{
		"type": "tabs", "tabs": infos, "url": activeURL, "title": activeTitle,
	})
	ctl.hub.publishText(string(b))
}

// ---------- events ----------

type screencastFrame struct {
	Data      string `json:"data"`
	SessionID int    `json:"sessionId"`
}

func (ctl *controller) onEvent(env envelope) {
	// Browser-level events (downloads) carry no sessionId.
	if env.SessionID == "" {
		ctl.onBrowserEvent(env)
		return
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	var owner *tab
	for _, t := range ctl.tabs {
		if t.sessionID == env.SessionID {
			owner = t
			break
		}
	}
	if owner == nil {
		return
	}
	switch env.Method {
	case "Page.screencastFrame":
		if owner.id != ctl.active {
			return
		}
		var f screencastFrame
		if err := json.Unmarshal(env.Params, &f); err != nil {
			return
		}
		ctl.cdp.notify(owner.sessionID, "Page.screencastFrameAck", map[string]any{"sessionId": f.SessionID})
		jpg, err := base64.StdEncoding.DecodeString(f.Data)
		if err == nil && len(jpg) > 0 {
			ctl.hub.publish(jpg)
		}
	case "Page.frameNavigated":
		var p struct {
			Frame struct {
				URL      string `json:"url"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		if err := json.Unmarshal(env.Params, &p); err == nil && p.Frame.ParentID == "" {
			owner.url = p.Frame.URL
			ctl.sendTabState()
			if owner.id == ctl.active {
				ctl.scheduleSave(p.Frame.URL)
			}
		}
	case "Page.loadEventFired":
		go ctl.refreshTitle(owner)
	case "Page.fileChooserOpened":
		var p struct {
			BackendNodeID int `json:"backendNodeId"`
		}
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return
		}
		req := &chooserReq{sessionID: env.SessionID, nodeID: p.BackendNodeID}
		if ctl.lastUpload != "" && time.Since(ctl.lastUploadAt) < 5*time.Minute {
			ctl.applyUploadLocked(req, ctl.lastUpload)
		} else {
			ctl.pendingChooser = req
			ctl.hub.publishText(`{"type":"chooser"}`)
		}
	}
}

func (ctl *controller) refreshTitle(t *tab) {
	res, err := ctl.tabCall(t, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true})
	if err != nil {
		return
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return
	}
	ctl.mu.Lock()
	if cur, ok := ctl.tabs[t.id]; ok && cur.title != r.Result.Value {
		cur.title = r.Result.Value
		ctl.sendTabState()
	}
	ctl.mu.Unlock()
}

func (ctl *controller) scheduleSave(u string) {
	if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "chrome") || strings.HasPrefix(u, "data:") {
		return
	}
	if ctl.saveTimer != nil {
		ctl.saveTimer.Stop()
	}
	ctl.saveTimer = time.AfterFunc(time.Second, func() { saveState(savedState{LastURL: u}) })
}

func (ctl *controller) onBrowserEvent(env envelope) {
	switch env.Method {
	case "Target.targetCreated":
		var p struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		if err := json.Unmarshal(env.Params, &p); err != nil || p.TargetInfo.Type != "page" {
			return
		}
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		if ctl.known[p.TargetInfo.TargetID] {
			return // one of ours (openTabLocked)
		}
		ctl.known[p.TargetInfo.TargetID] = true
		// A page opened its own tab (target=_blank): adopt and show it.
		if _, err := ctl.attachLocked(p.TargetInfo.TargetID); err == nil {
			ctl.activateLocked(p.TargetInfo.TargetID)
		}
	case "Target.targetDestroyed":
		var p struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return
		}
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		if _, ok := ctl.tabs[p.TargetID]; ok && len(ctl.order) > 1 {
			ctl.closeTabLocked(p.TargetID)
		}
	case "Browser.downloadWillBegin":
		var p struct {
			GUID              string `json:"guid"`
			SuggestedFilename string `json:"suggestedFilename"`
		}
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return
		}
		ctl.mu.Lock()
		ctl.downloads[p.GUID] = p.SuggestedFilename
		ctl.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"type": "download", "state": "begin", "name": p.SuggestedFilename})
		ctl.hub.publishText(string(b))
	case "Browser.downloadProgress":
		var p struct {
			GUID  string `json:"guid"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return
		}
		if p.State == "completed" || p.State == "canceled" {
			ctl.mu.Lock()
			name := ctl.downloads[p.GUID]
			ctl.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"type": "download", "state": p.State, "name": name})
			ctl.hub.publishText(string(b))
		}
	}
}

// ---------- uploads ----------

func (ctl *controller) onUpload(path string) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	ctl.lastUpload = path
	ctl.lastUploadAt = time.Now()
	if ctl.pendingChooser != nil {
		ctl.applyUploadLocked(ctl.pendingChooser, path)
		ctl.pendingChooser = nil
	}
}

func (ctl *controller) applyUploadLocked(req *chooserReq, path string) {
	_, err := ctl.cdp.call(req.sessionID, "DOM.setFileInputFiles", map[string]any{
		"backendNodeId": req.nodeID, "files": []string{path},
	})
	if err != nil {
		log.Printf("setFileInputFiles: %v", err)
		return
	}
	ctl.hub.publishText(`{"type":"chooser_done"}`)
}

// ---------- client input ----------

func (ctl *controller) handleClientMsg(m clientMsg) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	t := ctl.activeTab()
	switch m.Type {
	case "tab_new":
		_, _ = ctl.openTabLocked(localStartURL(ctl.cfg.listen))
		return
	case "tab_switch":
		ctl.activateLocked(m.TabID)
		return
	case "tab_close":
		ctl.closeTabLocked(m.TabID)
		return
	case "quality":
		if m.Quality >= 20 && m.Quality <= 95 && m.Quality != ctl.quality {
			ctl.quality = m.Quality
			if ctl.casting {
				ctl.stopScreencastLocked()
				if err := ctl.startScreencastLocked(); err == nil {
					ctl.casting = true
				}
			}
		}
		return
	case "zoom":
		if m.DSF >= 0.5 && m.DSF <= 3 && m.DSF != ctl.dsf {
			ctl.dsf = m.DSF
			if t != nil {
				_ = ctl.applyViewport(t)
				if ctl.casting {
					ctl.stopScreencastLocked()
					if err := ctl.startScreencastLocked(); err == nil {
						ctl.casting = true
					}
				}
			}
		}
		return
	case "home":
		if t != nil {
			_, _ = ctl.tabCall(t, "Page.navigate", map[string]any{"url": localStartURL(ctl.cfg.listen)})
		}
		return
	case "clipread":
		go ctl.readClipboard()
		return
	}
	if t == nil {
		return
	}
	switch m.Type {
	case "mousedown", "mouseup", "mousemove":
		typ := map[string]string{
			"mousedown": "mousePressed", "mouseup": "mouseReleased", "mousemove": "mouseMoved",
		}[m.Type]
		p := map[string]any{
			"type": typ, "x": m.X * float64(ctl.width), "y": m.Y * float64(ctl.height),
			"buttons": m.Buttons, "clickCount": m.Clicks,
		}
		if m.Button != "" {
			p["button"] = m.Button
		}
		_, _ = ctl.tabCall(t, "Input.dispatchMouseEvent", p)
	case "wheel":
		_, _ = ctl.tabCall(t, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": m.X * float64(ctl.width), "y": m.Y * float64(ctl.height),
			"deltaX": m.DeltaX, "deltaY": m.DeltaY, "modifiers": m.Modifiers,
		})
	case "keydown", "keyup":
		ctl.key(t, m, m.Type == "keydown")
	case "text":
		_, _ = ctl.tabCall(t, "Input.insertText", map[string]any{"text": m.Text})
	case "navigate":
		if m.URL != "" {
			_, _ = ctl.tabCall(t, "Page.navigate", map[string]any{"url": m.URL})
		}
	case "back":
		_, _ = ctl.tabCall(t, "Runtime.evaluate", map[string]any{"expression": "history.back()"})
	case "forward":
		_, _ = ctl.tabCall(t, "Runtime.evaluate", map[string]any{"expression": "history.forward()"})
	case "reload":
		_, _ = ctl.tabCall(t, "Page.reload", nil)
	case "resize":
		if m.Width >= 320 && m.Height >= 240 && (m.Width != ctl.width || m.Height != ctl.height) {
			ctl.width, ctl.height = m.Width, m.Height
			wasCasting := ctl.casting
			if wasCasting {
				ctl.stopScreencastLocked()
			}
			_ = ctl.applyViewport(t)
			if wasCasting {
				if err := ctl.startScreencastLocked(); err == nil {
					ctl.casting = true
				}
			}
		}
	}
}

func (ctl *controller) key(t *tab, m clientMsg, down bool) {
	// Printable character without command modifiers: a "char" event types it
	// exactly, whatever the viewer's keyboard layout is.
	if down && m.Text != "" && m.Modifiers&0xA == 0 { // no Ctrl(2)/Meta(8)
		_, _ = ctl.tabCall(t, "Input.dispatchKeyEvent", map[string]any{
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
	if down && m.Key == "Enter" {
		p["text"] = "\r"
		p["unmodifiedText"] = "\r"
	}
	_, _ = ctl.tabCall(t, "Input.dispatchKeyEvent", p)
}

func (ctl *controller) readClipboard() {
	ctl.mu.Lock()
	t := ctl.activeTab()
	tabURL := ""
	if t != nil {
		tabURL = t.url
	}
	ctl.mu.Unlock()
	if t == nil {
		return
	}
	// Grant clipboard permission for the current origin, then read.
	if u, err := url.Parse(tabURL); err == nil && u.Scheme != "" && u.Host != "" {
		_, _ = ctl.cdp.call("", "Browser.grantPermissions", map[string]any{
			"origin": u.Scheme + "://" + u.Host,
			"permissions": []string{"clipboardReadWrite", "clipboardSanitizedWrite"},
		})
	}
	res, err := ctl.tabCall(t, "Runtime.evaluate", map[string]any{
		"expression": "navigator.clipboard.readText()", "awaitPromise": true, "returnByValue": true,
	})
	if err != nil {
		return
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.Result.Value == "" {
		return
	}
	b, _ := json.Marshal(map[string]any{"type": "clipboard", "text": r.Result.Value})
	ctl.hub.publishText(string(b))
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
