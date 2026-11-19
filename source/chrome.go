package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const captureEvery = 3 * time.Minute

// running is a profile whose Chrome is open. cmd is nil when Chrome was
// already open before the manager started (we only re-attached to it).
type running struct {
	cmd       *exec.Cmd
	fwd       *Forwarder
	firstOpen bool // first time this profile runs on this computer
}

// Manager launches and closes Chrome, one separate user-data-dir per profile.
type Manager struct {
	store *Store
	sync  *Syncer

	mu          sync.Mutex
	run         map[string]*running
	lastCapture map[string]time.Time
	capturing   map[string]bool
	closing     map[string]bool
	headless    map[string]bool // invisible Chrome saving cookies right now
}

func NewManager(s *Store, sy *Syncer) *Manager {
	m := &Manager{store: s, sync: sy, run: map[string]*running{},
		lastCapture: map[string]time.Time{}, capturing: map[string]bool{}, closing: map[string]bool{},
		headless: map[string]bool{}}
	sy.running = m.RunningIDs
	sy.removed = m.removeLocal
	m.reattach()
	go m.watch()
	go m.uploadMissing()
	return m
}

// saveAfterExit saves the cookies of a profile whose Chrome was closed
// without the app's Close button (e.g. the window's X), by starting it
// invisibly for a moment.
func (m *Manager) saveAfterExit(id string) {
	if !m.sync.Enabled() {
		return
	}
	time.Sleep(2 * time.Second)
	if m.IsRunning(id) {
		return
	}
	if _, err := os.Stat(m.store.ProfileDir(id)); err != nil {
		return
	}
	chrome := m.ChromePath()
	if chrome == "" {
		return
	}
	m.mu.Lock()
	if m.headless[id] {
		m.mu.Unlock()
		return
	}
	m.headless[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.headless, id)
		m.mu.Unlock()
	}()
	p, _ := m.store.Get(id)
	if err := m.uploadHeadless(chrome, id); err != nil {
		log.Printf("LỖI tự lưu đăng nhập sau khi đóng %s: %v", id, err)
		m.sync.setCaptureErr(fmt.Sprintf("Không lưu được đăng nhập của \"%s\": %v", p.Name, err))
		return
	}
	log.Printf("đã tự lưu đăng nhập của %s sau khi Chrome đóng", id)
}

// uploadMissing runs once after start: profiles that have Chrome data on
// this computer but nothing on the server get their cookies uploaded.
func (m *Manager) uploadMissing() {
	time.Sleep(8 * time.Second)
	for i := 0; i < 30 && !m.sync.Enabled(); i++ {
		time.Sleep(10 * time.Second)
	}
	if !m.sync.Enabled() || m.sync.SyncOnce() != nil {
		return
	}
	cloud := m.sync.Status().Cloud
	for _, p := range m.store.Profiles() {
		if _, ok := cloud[p.ID]; ok {
			continue
		}
		m.saveAfterExit(p.ID)
	}
}

// reattach handles profiles whose Chrome is still open from a previous run
// of the manager: restart their auth-proxy forwarder on the same port.
func (m *Manager) reattach() {
	for _, p := range m.store.Profiles() {
		if !profileLocked(m.store.ProfileDir(p.ID)) {
			continue
		}
		r := &running{}
		if p.Proxy.Enabled() && p.Proxy.HasAuth() && p.FwdPort != 0 {
			if f, err := StartForwarder(p.Proxy, p.FwdPort); err == nil {
				if f.Port() == p.FwdPort {
					r.fwd = f
				} else {
					f.Close() // port taken; Chrome would not find it anyway
				}
			}
		}
		m.run[p.ID] = r
	}
}

// watch drops re-attached entries once Chrome is closed and saves the
// cookies of open profiles every few minutes.
func (m *Manager) watch() {
	for range time.Tick(3 * time.Second) {
		var ended, due []string
		m.mu.Lock()
		for id, r := range m.run {
			if r.cmd == nil && !profileLocked(m.store.ProfileDir(id)) {
				if r.fwd != nil {
					r.fwd.Close()
				}
				delete(m.run, id)
				ended = append(ended, id)
				continue
			}
			if time.Since(m.lastCapture[id]) >= captureEvery && !m.capturing[id] && !m.closing[id] {
				due = append(due, id)
			}
		}
		m.mu.Unlock()
		for _, id := range ended {
			go func(id string) {
				m.recordExtensions(id, false)
				m.saveAfterExit(id)
				m.sync.ReleaseLock(id)
			}(id)
		}
		if m.sync.Enabled() {
			for _, id := range due {
				go func(id string) { _ = m.Capture(id) }(id)
			}
		}
	}
}

func (m *Manager) RunningIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.run))
	for id := range m.run {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) ChromePath() string {
	if p := strings.TrimSpace(m.store.Settings().ChromePath); p != "" {
		if fileExists(p) {
			return p
		}
	}
	for _, p := range chromeCandidates() {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// IsRunning reports whether Chrome is open for this profile, whether we
// launched it or not.
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	_, ok := m.run[id]
	m.mu.Unlock()
	if ok {
		return true
	}
	return profileLocked(m.store.ProfileDir(id))
}

func normURL(u string) string {
	u = strings.TrimSpace(u)
	if u != "" && !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") && !strings.HasPrefix(u, "chrome:") {
		u = "https://" + u
	}
	return u
}

// Open launches Chrome for a profile. With sync on, it first takes the
// profile's lock and restores cookies saved by another computer.
func (m *Manager) Open(id string, force bool) error {
	p, ok := m.store.Get(id)
	if !ok {
		return errors.New("không tìm thấy hồ sơ")
	}
	for i := 0; i < 60; i++ { // a background save may be using the profile
		m.mu.Lock()
		busy := m.headless[id]
		m.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if m.IsRunning(id) {
		return nil
	}
	chrome := m.ChromePath()
	if chrome == "" {
		return errors.New("không tìm thấy Chrome — vào Cài đặt để chọn đường dẫn chrome.exe")
	}
	dir := m.store.ProfileDir(id)
	_, statErr := os.Stat(filepath.Join(dir, "Default"))
	firstOpen := statErr != nil
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	syncOn := m.sync.Enabled()
	var restore *CookieBundle
	var restoreVer int64
	if syncOn {
		multi := m.store.Settings().AllowMultiOpen
		if err := m.sync.TakeLock(id, force || multi); err != nil {
			var le *LockedError
			if errors.As(err, &le) {
				return err
			}
			log.Printf("lock %s: %v (opening anyway)", id, err)
		}
		b, v, err := m.sync.CookiesToRestore(id)
		if err != nil {
			log.Printf("restore %s: %v", id, err)
		}
		restore, restoreVer = b, v
		// Saved by this same computer: the local Chrome data is already
		// as new or newer, so do not overwrite it.
		if restore != nil && restore.DeviceID != "" && restore.DeviceID == m.sync.DeviceID() {
			m.sync.MarkRestored(id, restoreVer, restore)
			restore = nil
		}
		if restore != nil && len(restore.Bookmarks) > 0 {
			_ = os.MkdirAll(filepath.Join(dir, "Default"), 0o755)
			_ = os.WriteFile(filepath.Join(dir, "Default", "Bookmarks"), restore.Bookmarks, 0o644)
		}
	}

	args := []string{
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--window-name=" + p.Name,
		"--enable-logging", // writes chrome_debug.log in the profile folder (for diagnosing)
	}
	if syncOn {
		// Local-only DevTools port, used to save/restore the login cookies.
		_ = os.Remove(devToolsPortFile(dir))
		args = append(args, "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1")
	}
	var fwd *Forwarder
	if p.Proxy.Enabled() {
		var server string
		if p.Proxy.HasAuth() {
			f, err := StartForwarder(p.Proxy, p.FwdPort)
			if err != nil {
				return fmt.Errorf("không khởi tạo được proxy: %w", err)
			}
			fwd = f
			server = "http://" + f.Addr()
		} else {
			server = p.Proxy.URL().String()
		}
		args = append(args,
			"--proxy-server="+server,
			// Keep WebRTC on the proxy too so the real IP is not exposed.
			"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		)
	}
	startURL := normURL(p.StartURL)
	var extraTabs []string // Web Store pages of the extensions to install
	if firstOpen {
		for _, eid := range p.Extensions {
			extraTabs = append(extraTabs, webStoreURL(eid))
		}
	}
	if restore == nil {
		if startURL != "" {
			args = append(args, startURL)
		}
		args = append(args, extraTabs...)
	}

	cmd := exec.Command(chrome, args...)
	cmd.Dir = filepath.Dir(chrome)
	log.Printf("mở %s (%s) sync=%v restore=%v chrome=%s", id, p.Name, syncOn, restore != nil, chrome)
	if err := cmd.Start(); err != nil {
		if fwd != nil {
			fwd.Close()
		}
		m.sync.ReleaseLock(id)
		return fmt.Errorf("không mở được Chrome: %w", err)
	}
	r := &running{cmd: cmd, fwd: fwd, firstOpen: firstOpen}
	m.mu.Lock()
	m.run[id] = r
	m.lastCapture[id] = time.Now().Add(-captureEvery + time.Minute) // first save after 1 minute
	m.mu.Unlock()

	now := time.Now()
	_, _ = m.store.UpdateLocal(id, func(pp *Profile) {
		pp.LastOpened = &now
		if fwd != nil {
			pp.FwdPort = fwd.Port()
		}
	})

	if restore != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			c, err := dialCDP(ctx, dir, 30*time.Second)
			if err != nil {
				log.Printf("restore %s: %v", id, err)
				return
			}
			defer c.Close()
			if err := setCookies(ctx, c, restore.Cookies); err != nil {
				log.Printf("restore %s: %v", id, err)
				return
			}
			log.Printf("đã khôi phục %d cookie cho %s (bản %d)", len(restore.Cookies), id, restoreVer)
			m.sync.MarkRestored(id, restoreVer, restore)
			if startURL != "" {
				_ = openTabReplacing(ctx, c, startURL)
			}
			for _, u := range extraTabs {
				_ = c.Call(ctx, "Target.createTarget", map[string]any{"url": u}, nil)
			}
		}()
	}

	go func() {
		werr := cmd.Wait()
		log.Printf("Chrome của %s đã thoát (%v)", id, werr)
		m.mu.Lock()
		if m.run[id] == r {
			delete(m.run, id)
		}
		byApp := m.closing[id]
		delete(m.closing, id)
		m.mu.Unlock()
		m.recordExtensions(id, r.firstOpen)
		if fwd != nil {
			fwd.Close()
		}
		if !byApp {
			m.saveAfterExit(id) // closed with the window's X: save now
		}
		m.sync.ReleaseLock(id)
	}()
	return nil
}

// Capture reads the profile's cookies (and bookmarks) from the running
// Chrome and uploads them if they changed.
func (m *Manager) Capture(id string) error {
	if !m.sync.Enabled() {
		return nil
	}
	err := m.capture(id)
	p, _ := m.store.Get(id)
	if err != nil {
		log.Printf("LỖI lưu đăng nhập %s (%s): %v", id, p.Name, err)
		m.sync.setCaptureErr(fmt.Sprintf("Không lưu được đăng nhập của \"%s\": %v", p.Name, err))
	} else {
		m.sync.setCaptureErr("")
	}
	return err
}

func (m *Manager) capture(id string) error {
	m.mu.Lock()
	if m.capturing[id] {
		m.mu.Unlock()
		return nil
	}
	m.capturing[id] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.capturing, id)
		m.lastCapture[id] = time.Now()
		m.mu.Unlock()
	}()

	dir := m.store.ProfileDir(id)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dialCDP(ctx, dir, 3*time.Second)
	if err != nil {
		return fmt.Errorf("%w (hồ sơ được mở trước khi bật đồng bộ? Hãy đóng rồi mở lại)", err)
	}
	defer c.Close()
	b, err := m.bundleFrom(ctx, c, dir)
	if err != nil {
		return err
	}
	if err := m.sync.UploadCookies(id, b, false); err != nil {
		return err
	}
	log.Printf("đã lưu đăng nhập %s: %d cookie", id, len(b.Cookies))
	return nil
}

func (m *Manager) bundleFrom(ctx context.Context, c *cdpConn, dir string) (*CookieBundle, error) {
	cookies, err := getCookies(ctx, c)
	if err != nil {
		return nil, err
	}
	b := &CookieBundle{Cookies: cookies, CapturedAt: time.Now()}
	if bm, err := os.ReadFile(filepath.Join(dir, "Default", "Bookmarks")); err == nil && len(bm) < 8<<20 {
		b.Bookmarks = bm
	}
	return b, nil
}

// Close saves cookies, then asks Chrome to close cleanly. It returns at
// once; the UI sees the result through the status polling.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	if m.closing[id] {
		m.mu.Unlock()
		return nil
	}
	m.closing[id] = true
	r := m.run[id]
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.closing, id)
			m.mu.Unlock()
		}()
		dir := m.store.ProfileDir(id)
		if m.sync.Enabled() {
			if err := m.Capture(id); err != nil {
				log.Printf("capture %s before close: %v", id, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if c, err := dialCDP(ctx, dir, time.Second); err == nil {
				err = browserClose(ctx, c)
				c.Close()
				log.Printf("đóng %s: gửi lệnh đóng êm cho Chrome (lỗi=%v)", id, err)
			} else {
				log.Printf("đóng %s: không kết nối được Chrome: %v", id, err)
			}
			cancel()
			// Give Chrome time to write cookies to disk before any force-kill.
			for i := 0; i < 50 && profileLocked(dir); i++ {
				time.Sleep(400 * time.Millisecond)
			}
			if !profileLocked(dir) {
				log.Printf("đóng %s: Chrome đã tự đóng êm", id)
				return
			}
			log.Printf("đóng %s: Chrome chưa đóng sau 20 giây, chuyển sang đóng bằng taskkill", id)
		}
		if r != nil && r.cmd != nil && r.cmd.Process != nil {
			log.Printf("đóng %s: taskkill pid %d", id, r.cmd.Process.Pid)
			closeProcessTree(r.cmd.Process.Pid)
			return
		}
		log.Printf("đóng %s: tìm Chrome theo thư mục hồ sơ", id)
		if err := closeByUserDataDir(dir); err != nil {
			log.Printf("close %s: %v", id, err)
		}
	}()
	return nil
}

// UploadAll saves the cookies of every closed profile by starting its Chrome
// invisibly (headless) for a moment. Used after first enabling sync.
func (m *Manager) UploadAll() (int, error) {
	if !m.sync.Enabled() {
		return 0, errors.New("chưa đăng nhập đồng bộ")
	}
	chrome := m.ChromePath()
	if chrome == "" {
		return 0, errors.New("không tìm thấy Chrome")
	}
	var todo []Profile
	for _, p := range m.store.Profiles() {
		if _, err := os.Stat(m.store.ProfileDir(p.ID)); err == nil && !m.IsRunning(p.ID) {
			todo = append(todo, p)
		}
	}
	defer m.sync.setBusy("")
	done := 0
	var firstErr error
	for i, p := range todo {
		m.sync.setBusy(fmt.Sprintf("Đang tải cookie lên %d/%d: %s", i+1, len(todo), p.Name))
		if err := m.uploadHeadless(chrome, p.ID); err != nil {
			log.Printf("upload %s: %v", p.ID, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p.Name, err)
			}
			continue
		}
		done++
	}
	for _, id := range m.RunningIDs() {
		if m.Capture(id) == nil {
			done++
		}
	}
	return done, firstErr
}

func (m *Manager) uploadHeadless(chrome, id string) error {
	dir := m.store.ProfileDir(id)
	_ = os.Remove(devToolsPortFile(dir))
	cmd := hidden(exec.Command(chrome, "--headless=new", "--user-data-dir="+dir, "--no-first-run",
		"--no-default-browser-check", "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "about:blank"))
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
			closeProcessTree(cmd.Process.Pid)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := dialCDP(ctx, dir, 25*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	b, err := m.bundleFrom(ctx, c, dir)
	if err == nil {
		err = m.sync.UploadCookies(id, b, false)
	}
	_ = browserClose(ctx, c)
	return err
}

// removeLocal deletes the Chrome data of profiles deleted on another computer.
func (m *Manager) removeLocal(ids []string) {
	for _, id := range ids {
		if m.IsRunning(id) {
			continue
		}
		_ = os.RemoveAll(m.store.ProfileDir(id))
	}
}

// Shutdown is called when the manager window closes: save the cookies of
// open profiles and push pending changes.
func (m *Manager) Shutdown() {
	if !m.sync.Enabled() {
		return
	}
	var wg sync.WaitGroup
	for _, id := range m.RunningIDs() {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _ = m.Capture(id) }(id)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); _ = m.sync.SyncOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
}

// recordExtensions saves which Web Store extensions the profile has, so
// they can be offered on other computers. On a first run the list is only
// extended (the user may not have installed them yet).
func (m *Manager) recordExtensions(id string, firstOpen bool) {
	p, ok := m.store.Get(id)
	if !ok {
		return
	}
	local := InstalledExtensions(m.store.ProfileDir(id))
	next := local
	if firstOpen || len(local) == 0 {
		next = unionIDs(p.Extensions, local)
	}
	if strings.Join(next, ",") == strings.Join(validExtIDs(p.Extensions), ",") {
		return
	}
	_, _ = m.store.Update(id, func(pp *Profile) { pp.Extensions = next })
}

// Clone copies a closed profile: extensions, their data and Chrome
// settings, but not cookies/logins/history.
func (m *Manager) Clone(srcID string, names []string, group string, copyProxy bool) ([]Profile, error) {
	src, ok := m.store.Get(srcID)
	if !ok {
		return nil, errors.New("không tìm thấy hồ sơ gốc")
	}
	if m.IsRunning(srcID) {
		return nil, errors.New("hãy đóng hồ sơ gốc trước khi nhân bản")
	}
	srcDir := m.store.ProfileDir(srcID)
	exts := unionIDs(src.Extensions, InstalledExtensions(srcDir))
	var made []Profile
	now := time.Now()
	for _, name := range names {
		p := Profile{ID: newID(), Name: name, Group: group, Notes: src.Notes, StartURL: src.StartURL,
			Extensions: exts, CreatedAt: now}
		if copyProxy {
			p.Proxy = src.Proxy
		}
		if err := CloneProfileDir(srcDir, m.store.ProfileDir(p.ID)); err != nil {
			_ = os.RemoveAll(m.store.ProfileDir(p.ID))
			return made, err
		}
		if err := m.store.Add([]Profile{p}); err != nil {
			return made, err
		}
		made = append(made, p)
	}
	log.Printf("nhân bản %s thành %d hồ sơ (%d tiện ích)", srcID, len(made), len(exts))
	return made, nil
}
