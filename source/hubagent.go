package main

// Agent side of "Trung tâm Zalo": while someone is watching /hub on the
// VPS, stream screenshots of the chosen profiles' Zalo tab and replay the
// viewer's clicks/keys into those tabs (Chrome DevTools Protocol).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const zaloURL = "https://chat.zalo.me/"

type HubSettings struct {
	Enabled  bool     `json:"enabled"`
	Profiles []string `json:"profiles"`
}

type hubStream struct {
	id     string
	c      *cdpConn
	cancel context.CancelFunc
	mu     sync.Mutex
	frame  []byte // latest frame message
	seq    int
	sent   int
	title  string
	in     chan hubCmd // viewer input, handled one by one in order
}

type HubAgent struct {
	mgr   *Manager
	store *Store
	sync  *Syncer

	mu        sync.Mutex
	viewers   int
	connected bool
	lastErr   string
	streams   map[string]*hubStream
	out       [][]byte
	panelsSig string
	cancel    context.CancelFunc
	gen       int
	kick      chan struct{}
	wake      chan struct{}        // a new frame or message is waiting
	hot       map[string]time.Time // panels a viewer looked at or typed in
	tickNow   chan struct{}        // check the streams right away
	pending   map[string][]hubCmd  // input that came before its stream was ready
}

func NewHubAgent(m *Manager, s *Store, sy *Syncer) *HubAgent {
	a := &HubAgent{mgr: m, store: s, sync: sy, streams: map[string]*hubStream{}, kick: make(chan struct{}, 1),
		wake: make(chan struct{}, 1), hot: map[string]time.Time{},
		tickNow: make(chan struct{}, 1), pending: map[string][]hubCmd{}}
	go a.connLoop()
	go a.tickLoop()
	go a.sendLoop()
	return a
}

func (a *HubAgent) settings() HubSettings { return a.store.Settings().Hub }

func (a *HubAgent) isHubProfile(id string) bool {
	st := a.settings()
	if !st.Enabled {
		return false
	}
	for _, p := range st.Profiles {
		if p == id {
			return true
		}
	}
	return false
}

// Reconnect is called when the settings change.
func (a *HubAgent) Reconnect() {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.panelsSig = ""
	a.mu.Unlock()
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

type HubStatus struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Viewers   int    `json:"viewers"`
	Error     string `json:"error,omitempty"`
	URL       string `json:"url"`
}

func (a *HubAgent) Status() HubStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	srv, _, _ := a.sync.creds()
	return HubStatus{Enabled: a.settings().Enabled, Connected: a.connected, Viewers: a.viewers, Error: a.lastErr, URL: srv + "/hub/"}
}

// ---------- connection to the VPS ----------

func (a *HubAgent) connLoop() {
	backoff := time.Second
	for {
		if !a.settings().Enabled || !a.sync.Enabled() {
			a.setConnected(false, "")
			a.stopAll()
			select {
			case <-a.kick:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		err := a.listen()
		a.setConnected(false, "")
		a.stopAll()
		if err != nil && !errors.Is(err, context.Canceled) {
			a.setConnected(false, err.Error())
			log.Printf("hub: %v", err)
		}
		select {
		case <-a.kick:
			backoff = time.Second
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (a *HubAgent) setConnected(c bool, errMsg string) {
	a.mu.Lock()
	a.connected = c
	if errMsg != "" || c {
		a.lastErr = errMsg
	}
	if !c {
		a.viewers = 0
	}
	a.mu.Unlock()
}

func (a *HubAgent) listen() error {
	srv, tok, dev := a.sync.creds()
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.gen++
	a.mu.Unlock()
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv+"/api/hub/agent", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Device", dev)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("không kết nối được máy chủ: %s", friendlyNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return errors.New("máy chủ chưa có tính năng Trung tâm Zalo (cần cập nhật máy chủ)")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("máy chủ trả lỗi %d", resp.StatusCode)
	}
	a.setConnected(true, "")
	log.Printf("hub: đã kết nối máy chủ")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		a.handle(bytes.TrimPrefix(line, []byte("data: ")))
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("máy chủ đóng kết nối")
}

func (a *HubAgent) push(v any) {
	b, _ := json.Marshal(v)
	a.mu.Lock()
	a.out = append(a.out, b)
	a.mu.Unlock()
	a.wakeUp()
}

func (a *HubAgent) wakeUp() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// isHot: the panel a viewer is using gets every frame right away; the
// others only a picture every couple of seconds, so typing stays quick.
func (a *HubAgent) isHotLocked(id string) bool {
	if len(a.hot) == 0 {
		return true
	}
	t, ok := a.hot[id]
	return ok && time.Since(t) < 3*time.Minute
}

// sendLoop posts queued messages and new frames as soon as they exist.
func (a *HubAgent) sendLoop() {
	client := &http.Client{Timeout: 30 * time.Second}
	tick := time.NewTicker(500 * time.Millisecond)
	lastSent := map[string]time.Time{}
	for {
		select {
		case <-a.wake:
		case <-tick.C:
		}
		a.mu.Lock()
		if !a.connected {
			a.out = nil
			a.mu.Unlock()
			continue
		}
		msgs := a.out
		a.out = nil
		streams := make([]*hubStream, 0, len(a.streams))
		hot := map[string]bool{}
		for id, s := range a.streams {
			streams = append(streams, s)
			hot[id] = a.isHotLocked(id)
		}
		a.mu.Unlock()
		for _, s := range streams {
			s.mu.Lock()
			if s.frame != nil && s.seq != s.sent && (hot[s.id] || s.sent < 0 || time.Since(lastSent[s.id]) > 2*time.Second) {
				msgs = append(msgs, s.frame)
				s.sent = s.seq
				lastSent[s.id] = time.Now()
			}
			s.mu.Unlock()
		}
		if len(msgs) == 0 {
			continue
		}
		srv, tok, dev := a.sync.creds()
		req, _ := http.NewRequest("POST", srv+"/api/hub/agent/push", bytes.NewReader(bytes.Join(msgs, []byte("\n"))))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Device", dev)
		req.Header.Set("Content-Type", "application/x-ndjson")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
		time.Sleep(25 * time.Millisecond) // at most ~40 posts a second
	}
}

// ---------- viewer commands ----------

type hubCmd struct {
	T       string  `json:"t"`
	N       int     `json:"n"`
	P       string  `json:"p"`
	Type    string  `json:"type"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Button  string  `json:"button"`
	Clicks  int     `json:"clicks"`
	DX      float64 `json:"dx"`
	DY      float64 `json:"dy"`
	Mods    int     `json:"mods"`
	Key     string  `json:"key"`
	Code    string  `json:"code"`
	KeyCode int     `json:"keyCode"`
	Text    string  `json:"text"`
}

func (a *HubAgent) handle(raw []byte) {
	var c hubCmd
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	switch c.T {
	case "viewers":
		a.mu.Lock()
		a.viewers = c.N
		a.panelsSig = ""
		a.mu.Unlock()
		a.checkNow()
		if c.N == 0 {
			a.stopAll()
		}
		return
	case "hello":
		a.mu.Lock()
		a.panelsSig = ""
		for _, s := range a.streams {
			s.mu.Lock()
			s.sent = -1 // resend the current picture
			s.mu.Unlock()
		}
		a.mu.Unlock()
		return
	}
	if !a.isHubProfile(c.P) {
		return
	}
	a.mu.Lock()
	a.hot[c.P] = time.Now() // looked at ("focus") or typed in
	a.mu.Unlock()
	if c.T == "focus" {
		a.wakeUp()
		return
	}
	switch c.T {
	case "open":
		go func() {
			if err := a.mgr.Open(c.P, false); err != nil {
				a.push(map[string]any{"t": "error", "p": c.P, "error": err.Error()})
			}
		}()
		return
	}
	a.mu.Lock()
	s := a.streams[c.P]
	a.mu.Unlock()
	if s == nil {
		// the picture is not streaming yet: keep the input until it is
		a.mu.Lock()
		if len(a.pending[c.P]) < 500 {
			a.pending[c.P] = append(a.pending[c.P], c)
		}
		a.mu.Unlock()
		a.checkNow()
		return
	}
	select {
	case s.in <- c:
	default: // far behind: drop
	}
}

// inputLoop applies the viewer's clicks and keys to one tab, in order.
func (s *hubStream) inputLoop() {
	for {
		select {
		case <-s.c.Done():
			return
		case c := <-s.in:
			s.apply(c)
		}
	}
}

func (s *hubStream) apply(c hubCmd) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	{
		switch c.T {
		case "mouse":
			p := map[string]any{"type": c.Type, "x": c.X, "y": c.Y, "modifiers": c.Mods}
			switch c.Type {
			case "mouseWheel":
				p["deltaX"], p["deltaY"] = c.DX, c.DY
			case "mousePressed", "mouseReleased":
				p["button"], p["clickCount"] = orDefault(c.Button, "left"), max(c.Clicks, 1)
			default:
				p["button"] = "none"
			}
			err = s.c.Call(ctx, "Input.dispatchMouseEvent", p, nil)
		case "key":
			p := map[string]any{"type": c.Type, "key": c.Key, "code": c.Code, "modifiers": c.Mods,
				"windowsVirtualKeyCode": c.KeyCode, "nativeVirtualKeyCode": c.KeyCode}
			if c.Text != "" {
				p["text"], p["unmodifiedText"] = c.Text, c.Text
			}
			err = s.c.Call(ctx, "Input.dispatchKeyEvent", p, nil)
		case "text":
			err = s.c.Call(ctx, "Input.insertText", map[string]any{"text": c.Text}, nil)
		case "zalo":
			err = s.c.Call(ctx, "Page.navigate", map[string]any{"url": zaloURL}, nil)
		case "reload":
			err = s.c.Call(ctx, "Page.reload", nil, nil)
		}
		if err != nil {
			log.Printf("hub %s %s: %v", c.P, c.T, err)
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---------- screen streams ----------

func (a *HubAgent) stopAll() {
	a.mu.Lock()
	streams := a.streams
	a.streams = map[string]*hubStream{}
	a.mu.Unlock()
	for _, s := range streams {
		s.cancel()
	}
}

func (a *HubAgent) stop(id string) {
	a.mu.Lock()
	s := a.streams[id]
	delete(a.streams, id)
	a.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

// tickLoop keeps streams in line with open profiles and publishes panels.
func (a *HubAgent) checkNow() {
	select {
	case a.tickNow <- struct{}{}:
	default:
	}
}

func (a *HubAgent) tickLoop() {
	tick := time.NewTicker(2 * time.Second)
	for {
		select {
		case <-tick.C:
		case <-a.tickNow:
		}
		a.mu.Lock()
		watching := a.connected && a.viewers > 0
		a.mu.Unlock()
		if !watching {
			a.mu.Lock()
			clear(a.pending)
			a.mu.Unlock()
			continue
		}
		st := a.settings()
		for _, id := range st.Profiles {
			running := a.mgr.IsRunning(id) && a.mgr.HasControl(id)
			if !running {
				a.mu.Lock()
				delete(a.pending, id)
				a.mu.Unlock()
			}
			a.mu.Lock()
			s := a.streams[id]
			a.mu.Unlock()
			alive := s != nil
			if alive {
				select {
				case <-s.c.Done():
					alive = false
				default:
				}
			}
			switch {
			case running && !alive:
				a.stop(id)
				if err := a.start(id); err != nil {
					log.Printf("hub stream %s: %v", id, err)
				}
			case !running && s != nil:
				a.stop(id)
			case alive:
				a.refreshTitle(s)
			}
		}
		a.publishPanels(st)
	}
}

var reUnread = regexp.MustCompile(`^\((\d+)\)`)

func (a *HubAgent) publishPanels(st HubSettings) {
	type panel struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Running bool   `json:"running"`
		Title   string `json:"title"`
		Unread  int    `json:"unread"`
	}
	var items []panel
	for _, id := range st.Profiles {
		p, ok := a.store.Get(id)
		if !ok || p.TrashedAt != nil {
			continue
		}
		it := panel{ID: id, Name: p.Name, Running: a.mgr.IsRunning(id)}
		a.mu.Lock()
		if s := a.streams[id]; s != nil {
			s.mu.Lock()
			it.Title = s.title
			s.mu.Unlock()
		}
		a.mu.Unlock()
		if m := reUnread.FindStringSubmatch(it.Title); m != nil {
			fmt.Sscan(m[1], &it.Unread)
		}
		items = append(items, it)
	}
	b, _ := json.Marshal(items)
	sig := string(b)
	a.mu.Lock()
	changed := sig != a.panelsSig
	a.panelsSig = sig
	a.mu.Unlock()
	if changed {
		a.push(map[string]any{"t": "panels", "items": items})
	}
}

func (a *HubAgent) refreshTitle(s *hubStream) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if s.c.Call(ctx, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &r) == nil {
		s.mu.Lock()
		s.title = r.Result.Value
		s.mu.Unlock()
	}
}

func (a *HubAgent) start(id string) error {
	dir := a.store.ProfileDir(id)
	ctx, cancel := context.WithCancel(context.Background())
	tctx, tcancel := context.WithTimeout(ctx, 15*time.Second)
	defer tcancel()
	b, err := dialCDP(tctx, dir, 3*time.Second)
	if err != nil {
		cancel()
		return err
	}
	pages, err := pageTargets(tctx, b)
	var target string
	for _, p := range pages {
		if strings.Contains(p.URL, "zalo.me") {
			target = p.TargetID
			break
		}
	}
	if target == "" && err == nil {
		// No Zalo tab yet: use the first tab and open Zalo in it.
		if len(pages) > 0 {
			target = pages[0].TargetID
		} else {
			var r struct {
				TargetID string `json:"targetId"`
			}
			_ = b.Call(tctx, "Target.createTarget", map[string]any{"url": zaloURL}, &r)
			target = r.TargetID
		}
	}
	b.Close()
	if target == "" {
		cancel()
		return errors.New("không tìm thấy tab")
	}
	c, err := dialPage(tctx, dir, target)
	if err != nil {
		cancel()
		return err
	}
	s := &hubStream{id: id, c: c, cancel: func() { cancel(); c.Close() }, in: make(chan hubCmd, 512)}
	go s.inputLoop()
	c.SetEventHandler(func(method string, params json.RawMessage) {
		if method != "Page.screencastFrame" {
			return
		}
		var f struct {
			Data     string `json:"data"`
			Session  int    `json:"sessionId"`
			Metadata struct {
				W float64 `json:"deviceWidth"`
				H float64 `json:"deviceHeight"`
			} `json:"metadata"`
		}
		if json.Unmarshal(params, &f) != nil {
			return
		}
		go func() { // acknowledge outside the read loop
			actx, acancel := context.WithTimeout(ctx, 3*time.Second)
			_ = c.Call(actx, "Page.screencastFrameAck", map[string]any{"sessionId": f.Session}, nil)
			acancel()
		}()
		msg, _ := json.Marshal(map[string]any{"t": "frame", "p": id, "d": f.Data, "w": f.Metadata.W, "h": f.Metadata.H})
		s.mu.Lock()
		s.frame = msg
		s.seq++
		s.mu.Unlock()
		a.wakeUp()
	})
	_ = c.Call(tctx, "Page.bringToFront", nil, nil)
	if u := currentURL(pages, target); u == "" || u == "about:blank" || strings.HasPrefix(u, "chrome://newtab") {
		_ = c.Call(tctx, "Page.navigate", map[string]any{"url": zaloURL}, nil)
	}
	if err := c.Call(tctx, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": 80, "maxWidth": 1920, "maxHeight": 1200, "everyNthFrame": 1,
	}, nil); err != nil {
		s.cancel()
		return err
	}
	a.refreshTitle(s)
	a.mu.Lock()
	a.streams[id] = s
	for _, c := range a.pending[id] {
		select {
		case s.in <- c:
		default:
		}
	}
	delete(a.pending, id)
	a.mu.Unlock()
	log.Printf("hub: đang truyền hình ảnh %s", id)
	return nil
}

func currentURL(pages []pageTarget, id string) string {
	for _, p := range pages {
		if p.TargetID == id {
			return p.URL
		}
	}
	return ""
}

// hubLaunchArgs keeps Chrome painting a hub profile even when its window is
// covered by other windows, so the remote picture stays live.
func hubLaunchArgs() []string {
	return []string{
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--disable-features=CalculateNativeWinOcclusion",
	}
}
