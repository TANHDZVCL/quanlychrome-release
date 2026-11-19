package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Minimal Chrome DevTools Protocol client. Chrome is started with
// --remote-debugging-port=0 and writes the chosen port to
// <user-data-dir>/DevToolsActivePort.

type cdpConn struct {
	ws      *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan cdpResp
	done    chan struct{}
	// onEvent, if set, receives protocol events (messages without an id).
	onEvent func(method string, params json.RawMessage)
}

type cdpResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func devToolsPortFile(dir string) string { return filepath.Join(dir, "DevToolsActivePort") }

func readDevToolsURL(dir string) (string, error) {
	b, err := os.ReadFile(devToolsPortFile(dir))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return "", errors.New("DevToolsActivePort chưa sẵn sàng")
	}
	return fmt.Sprintf("ws://127.0.0.1:%s%s", strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])), nil
}

// dialCDP connects to the browser endpoint of the Chrome using dir, waiting
// up to wait for Chrome to come up.
func dialCDP(ctx context.Context, dir string, wait time.Duration) (*cdpConn, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		u, err := readDevToolsURL(dir)
		if err == nil {
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ws, _, derr := websocket.Dial(dctx, u, nil)
			cancel()
			if derr == nil {
				ws.SetReadLimit(256 << 20)
				c := &cdpConn{ws: ws, pending: map[int64]chan cdpResp{}, done: make(chan struct{})}
				go c.readLoop()
				return c, nil
			}
			err = derr
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("không kết nối được Chrome: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *cdpConn) readLoop() {
	defer close(c.done)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			cdpResp
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID == 0 {
			c.mu.Lock()
			h := c.onEvent
			c.mu.Unlock()
			if h != nil && msg.Method != "" {
				h(msg.Method, msg.Params)
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg.cdpResp
		}
	}
}

func (c *cdpConn) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan cdpResp, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-c.done:
		return errors.New("Chrome đã đóng kết nối")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *cdpConn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// ---------- cookies ----------

type cookieList []map[string]any

func getCookies(ctx context.Context, c *cdpConn) (cookieList, error) {
	var res struct {
		Cookies cookieList `json:"cookies"`
	}
	if err := c.Call(ctx, "Storage.getCookies", map[string]any{}, &res); err != nil {
		return nil, err
	}
	sort.Slice(res.Cookies, func(i, j int) bool {
		a, b := res.Cookies[i], res.Cookies[j]
		ka := fmt.Sprint(a["domain"], "|", a["path"], "|", a["name"])
		kb := fmt.Sprint(b["domain"], "|", b["path"], "|", b["name"])
		return ka < kb
	})
	return res.Cookies, nil
}

// Fields of Network.Cookie that are also accepted by Network.CookieParam.
var cookieParamFields = []string{"name", "value", "domain", "path", "secure", "httpOnly",
	"sameSite", "expires", "priority", "sourceScheme", "sourcePort", "partitionKey"}

func setCookies(ctx context.Context, c *cdpConn, cookies cookieList) error {
	params := make([]map[string]any, 0, len(cookies))
	for _, ck := range cookies {
		p := map[string]any{}
		for _, f := range cookieParamFields {
			if v, ok := ck[f]; ok && v != nil {
				p[f] = v
			}
		}
		// Session cookies would vanish at the next Chrome restart on the
		// new computer; keep them for 30 days instead.
		sess, _ := ck["session"].(bool)
		if e, ok := ck["expires"].(float64); sess || !ok || e <= 0 {
			p["expires"] = float64(time.Now().Add(30 * 24 * time.Hour).Unix())
		}
		params = append(params, p)
	}
	// Send in chunks; one bad cookie must not drop all the others.
	for i := 0; i < len(params); i += 200 {
		end := min(i+200, len(params))
		if err := c.Call(ctx, "Storage.setCookies", map[string]any{"cookies": params[i:end]}, nil); err != nil {
			for _, one := range params[i:end] {
				_ = c.Call(ctx, "Storage.setCookies", map[string]any{"cookies": []any{one}}, nil)
			}
		}
	}
	return nil
}

func browserClose(ctx context.Context, c *cdpConn) error {
	err := c.Call(ctx, "Browser.close", nil, nil)
	if err != nil && strings.Contains(err.Error(), "đã đóng kết nối") {
		return nil // Chrome closed before answering
	}
	return err
}

// openTab opens url in a new tab and closes the tabs that existed before.
func openTabReplacing(ctx context.Context, c *cdpConn, url string) error {
	var before struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	_ = c.Call(ctx, "Target.getTargets", nil, &before)
	if err := c.Call(ctx, "Target.createTarget", map[string]any{"url": url}, nil); err != nil {
		return err
	}
	for _, t := range before.TargetInfos {
		if t.Type == "page" {
			_ = c.Call(ctx, "Target.closeTarget", map[string]any{"targetId": t.TargetID}, nil)
		}
	}
	return nil
}

// SetEventHandler installs the callback for protocol events.
func (c *cdpConn) SetEventHandler(h func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.onEvent = h
	c.mu.Unlock()
}

// Done is closed when the connection ends.
func (c *cdpConn) Done() <-chan struct{} { return c.done }

// devToolsPort returns the control port of a running profile.
func devToolsPort(dir string) (string, error) {
	b, err := os.ReadFile(devToolsPortFile(dir))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[0]), nil
}

// dialPage connects straight to one tab (page target) of a running profile.
func dialPage(ctx context.Context, dir, targetID string) (*cdpConn, error) {
	port, err := devToolsPort(dir)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, fmt.Sprintf("ws://127.0.0.1:%s/devtools/page/%s", port, targetID), nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(64 << 20)
	c := &cdpConn{ws: ws, pending: map[int64]chan cdpResp{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

type pageTarget struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

func pageTargets(ctx context.Context, c *cdpConn) ([]pageTarget, error) {
	var t struct {
		TargetInfos []pageTarget `json:"targetInfos"`
	}
	if err := c.Call(ctx, "Target.getTargets", nil, &t); err != nil {
		return nil, err
	}
	var out []pageTarget
	for _, ti := range t.TargetInfos {
		if ti.Type == "page" && !strings.HasPrefix(ti.URL, "devtools://") && !strings.HasPrefix(ti.URL, "chrome-extension://") {
			out = append(out, ti)
		}
	}
	return out, nil
}
