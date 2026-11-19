package main

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"
)

type winBounds struct {
	Left        int    `json:"left"`
	Top         int    `json:"top"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	WindowState string `json:"windowState,omitempty"`
}

// browserWindowID finds the Chrome window that shows the first tab.
func browserWindowID(ctx context.Context, c *cdpConn) (int, error) {
	var t struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := c.Call(ctx, "Target.getTargets", nil, &t); err != nil {
		return 0, err
	}
	for _, ti := range t.TargetInfos {
		if ti.Type != "page" {
			continue
		}
		var w struct {
			WindowID int `json:"windowId"`
		}
		if err := c.Call(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": ti.TargetID}, &w); err == nil {
			return w.WindowID, nil
		}
	}
	return 0, errors.New("không tìm thấy cửa sổ")
}

// Tile arranges the Chrome windows of the given (or all) open profiles in
// a grid filling the screen.
func (m *Manager) Tile(ids []string) (int, error) {
	if len(ids) == 0 {
		ids = m.RunningIDs()
	}
	type win struct {
		name string
		c    *cdpConn
		id   int
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wins []win
	for _, id := range ids {
		if !m.IsRunning(id) {
			continue
		}
		c, err := dialCDP(ctx, m.store.ProfileDir(id), 2*time.Second)
		if err != nil {
			continue // opened by an older version without the control port
		}
		wid, err := browserWindowID(ctx, c)
		if err != nil {
			c.Close()
			continue
		}
		p, _ := m.store.Get(id)
		wins = append(wins, win{p.Name, c, wid})
	}
	defer func() {
		for _, w := range wins {
			w.c.Close()
		}
	}()
	if len(wins) == 0 {
		return 0, errors.New("không có cửa sổ nào để xếp (hãy mở hồ sơ bằng phần mềm; hồ sơ mở từ bản cũ cần đóng và mở lại)")
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i].name < wins[j].name })

	// The maximized size of the first window = usable screen area.
	first := wins[0]
	_ = first.c.Call(ctx, "Browser.setWindowBounds", map[string]any{"windowId": first.id, "bounds": winBounds{WindowState: "maximized"}}, nil)
	time.Sleep(400 * time.Millisecond)
	var got struct {
		Bounds winBounds `json:"bounds"`
	}
	if err := first.c.Call(ctx, "Browser.getWindowBounds", map[string]any{"windowId": first.id}, &got); err != nil {
		return 0, err
	}
	area := got.Bounds
	if area.Width < 400 || area.Height < 300 {
		area = winBounds{Left: 0, Top: 0, Width: 1920, Height: 1040}
	}

	n := len(wins)
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := int(math.Ceil(float64(n) / float64(cols)))
	w, h := area.Width/cols, area.Height/rows
	for i, wn := range wins {
		r, col := i/cols, i%cols
		_ = wn.c.Call(ctx, "Browser.setWindowBounds", map[string]any{"windowId": wn.id, "bounds": winBounds{WindowState: "normal"}}, nil)
		_ = wn.c.Call(ctx, "Browser.setWindowBounds", map[string]any{"windowId": wn.id, "bounds": map[string]int{
			"left": area.Left + col*w, "top": area.Top + r*h, "width": w, "height": h}}, nil)
	}
	return n, nil
}
