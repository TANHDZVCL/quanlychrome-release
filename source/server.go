package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store *Store
	mgr   *Manager
	sync  *Syncer
	token string
}

type profileView struct {
	Profile
	Running bool `json:"running"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.state)
	api.HandleFunc("POST /api/profiles", s.create)
	api.HandleFunc("PUT /api/profiles/{id}", s.update)
	api.HandleFunc("POST /api/delete", s.delete)
	api.HandleFunc("POST /api/clone", s.clone)
	api.HandleFunc("POST /api/restore", s.restore)
	api.HandleFunc("POST /api/tile", s.tile)
	api.HandleFunc("POST /api/pin", s.pin)
	api.HandleFunc("POST /api/meta", s.saveMeta)
	api.HandleFunc("GET /api/totp/{id}", s.totp)
	api.HandleFunc("POST /api/proxy/check-all", s.checkAll)
	api.HandleFunc("POST /api/clear-cache", s.clearCache)
	api.HandleFunc("POST /api/open", s.open)
	api.HandleFunc("POST /api/close", s.close)
	api.HandleFunc("POST /api/folder/{id}", s.folder)
	api.HandleFunc("POST /api/proxy/check", s.checkProxy)
	api.HandleFunc("POST /api/proxy/assign", s.assignProxies)
	api.HandleFunc("POST /api/settings", s.saveSettings)
	api.HandleFunc("POST /api/sync/login", s.syncLogin)
	api.HandleFunc("POST /api/sync/logout", s.syncLogout)
	api.HandleFunc("POST /api/sync/now", s.syncNow)
	api.HandleFunc("POST /api/sync/upload-all", s.syncUploadAll)
	api.HandleFunc("POST /api/sync/export", s.syncExport)
	api.HandleFunc("POST /api/sync/import", s.syncImport)
	api.HandleFunc("POST /api/sync/password", s.syncPassword)
	api.HandleFunc("POST /api/open-path", s.openPath)
	api.HandleFunc("/api/vpn/", s.vpnPass)
	api.HandleFunc("GET /api/update/check", s.updateCheck)
	api.HandleFunc("POST /api/update/apply", s.updateApply)

	// The API only answers requests carrying the secret token, so web pages
	// opened inside the managed Chrome profiles cannot call it.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		api.ServeHTTP(w, r)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	profileSizes.Refresh(s.store, 2*time.Minute)
	ps := s.store.Profiles()
	views := make([]profileView, len(ps))
	for i, p := range ps {
		views[i] = profileView{Profile: p, Running: s.mgr.IsRunning(p.ID)}
	}
	writeJSON(w, map[string]any{
		"profiles":   views,
		"groups":     s.store.Groups(),
		"settings":   s.store.Settings(),
		"chromePath": s.mgr.ChromePath(),
		"dataDir":    s.store.dataDir,
		"sync":       s.sync.Status(),
		"sizes":      profileSizes.Get(),
		"version":    appVersion,
		"meta":       s.store.Meta(),
		"health":     health.For(ps),
	})
}

type profileInput struct {
	Name        string   `json:"name"`
	Group       string   `json:"group"`
	Notes       string   `json:"notes"`
	StartURL    string   `json:"startUrl"`
	Proxy       Proxy    `json:"proxy"`
	Count       int      `json:"count"`
	Extensions  []string `json:"extensions"`
	Account     *Account `json:"account"`
	SiteDomains *string  `json:"siteDomains"`
}

func cleanAccount(a *Account) *Account {
	if a == nil {
		return nil
	}
	c := &Account{UID: strings.TrimSpace(a.UID), Email: strings.TrimSpace(a.Email), Password: a.Password,
		TwoFA: strings.TrimSpace(a.TwoFA), Extra: a.Extra}
	if c.empty() {
		return nil
	}
	return c
}

func cleanProxy(p Proxy) (Proxy, error) {
	p.Type = strings.ToLower(strings.TrimSpace(p.Type))
	p.Host = strings.TrimSpace(p.Host)
	if p.Type == "" || p.Host == "" {
		return Proxy{}, nil
	}
	if p.Type != "http" && p.Type != "socks5" {
		return p, errors.New("loại proxy phải là HTTP hoặc SOCKS5")
	}
	if p.Port <= 0 || p.Port > 65535 {
		return p, errors.New("cổng proxy không hợp lệ")
	}
	return p, nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in profileInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, 400, "Hãy nhập tên hồ sơ")
		return
	}
	px, err := cleanProxy(in.Proxy)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Count < 1 {
		in.Count = 1
	}
	if in.Count > 200 {
		writeErr(w, 400, "Mỗi lần tạo tối đa 200 hồ sơ")
		return
	}
	now := time.Now()
	var ps []Profile
	for i := 1; i <= in.Count; i++ {
		name := in.Name
		if in.Count > 1 {
			name = fmt.Sprintf("%s %d", in.Name, i)
		}
		ps = append(ps, Profile{
			ID: newID(), Name: name, Group: strings.TrimSpace(in.Group), Notes: in.Notes,
			StartURL: strings.TrimSpace(in.StartURL), Proxy: px, CreatedAt: now,
			Extensions: validExtIDs(in.Extensions),
		})
		if in.Count == 1 {
			ps[len(ps)-1].Account = cleanAccount(in.Account)
		}
		if in.SiteDomains != nil {
			ps[len(ps)-1].SiteDomains = strings.Join(siteDomains(*in.SiteDomains), ", ")
		}
	}
	if err := s.store.Add(ps); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"created": len(ps)})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	var in profileInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, 400, "Hãy nhập tên hồ sơ")
		return
	}
	px, err := cleanProxy(in.Proxy)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p, err := s.store.Update(r.PathValue("id"), func(p *Profile) {
		p.Name, p.Group, p.Notes, p.StartURL = in.Name, strings.TrimSpace(in.Group), in.Notes, strings.TrimSpace(in.StartURL)
		p.Proxy = px
		if in.Extensions != nil {
			p.Extensions = validExtIDs(in.Extensions)
		}
		if in.SiteDomains != nil {
			p.SiteDomains = strings.Join(siteDomains(*in.SiteDomains), ", ")
		}
		if in.Account != nil { // absent = keep; {} = clear
			p.Account = cleanAccount(in.Account)
		}
	})
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, p)
}

type idsInput struct {
	IDs []string `json:"ids"`
}

// delete moves profiles to the trash, or deletes them for good when
// permanent is set (used from the trash).
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs       []string `json:"ids"`
		Permanent bool     `json:"permanent"`
	}
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	for _, id := range in.IDs {
		if s.mgr.IsRunning(id) {
			p, _ := s.store.Get(id)
			writeErr(w, 409, "Hãy đóng Chrome của hồ sơ \""+p.Name+"\" trước khi xóa")
			return
		}
	}
	if !in.Permanent {
		if err := s.store.SetTrashed(in.IDs, true); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"trashed": len(in.IDs)})
		return
	}
	if err := s.store.Delete(in.IDs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var failed int
	for _, id := range in.IDs {
		if err := os.RemoveAll(s.store.ProfileDir(id)); err != nil {
			failed++
		}
	}
	res := map[string]any{"deleted": len(in.IDs)}
	if failed > 0 {
		res["warning"] = fmt.Sprintf("%d thư mục dữ liệu chưa xóa được (có thể đang bị khóa)", failed)
	}
	writeJSON(w, res)
}

func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs   []string `json:"ids"`
		Force bool     `json:"force"`
	}
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	errs := map[string]string{}
	locked := map[string]string{}
	for i, id := range in.IDs {
		if i > 0 {
			time.Sleep(400 * time.Millisecond) // don't start many Chromes at the same instant
		}
		if err := s.mgr.Open(id, in.Force); err != nil {
			var le *LockedError
			if errors.As(err, &le) {
				locked[id] = err.Error()
				continue
			}
			errs[id] = err.Error()
		}
	}
	writeJSON(w, map[string]any{"errors": errs, "locked": locked})
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	var in idsInput
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	errs := map[string]string{}
	for _, id := range in.IDs {
		if !s.mgr.IsRunning(id) {
			continue
		}
		if err := s.mgr.Close(id); err != nil {
			errs[id] = err.Error()
		}
	}
	writeJSON(w, map[string]any{"errors": errs})
}

func (s *Server) folder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Get(id); !ok {
		writeErr(w, 404, "không tìm thấy hồ sơ")
		return
	}
	dir := s.store.ProfileDir(id)
	_ = os.MkdirAll(dir, 0o755)
	if err := openFolder(dir); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) checkProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Proxy Proxy  `json:"proxy"`
		ID    string `json:"id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	px := in.Proxy
	if in.ID != "" {
		p, ok := s.store.Get(in.ID)
		if !ok {
			writeErr(w, 404, "không tìm thấy hồ sơ")
			return
		}
		px = p.Proxy
	}
	px, err := cleanProxy(px)
	if err != nil {
		writeJSON(w, ProxyCheck{Error: err.Error()})
		return
	}
	c := CheckProxy(px)
	if in.ID != "" {
		health.Set(in.ID, px, c)
	}
	writeJSON(w, c)
}

func (s *Server) assignProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs  []string `json:"ids"`
		Text string   `json:"text"`
		Type string   `json:"type"`
	}
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	var list []Proxy
	var bad []string
	for i, line := range strings.Split(in.Text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := ParseProxyLine(line, in.Type)
		if err != nil {
			bad = append(bad, fmt.Sprintf("Dòng %d: %s", i+1, err.Error()))
			continue
		}
		list = append(list, p)
	}
	if len(bad) > 0 {
		writeErr(w, 400, strings.Join(bad, "\n"))
		return
	}
	// Empty list = remove the proxy from the selected profiles.
	for i, id := range in.IDs {
		var px Proxy
		if len(list) > 0 {
			px = list[i%len(list)]
		}
		if _, err := s.store.Update(id, func(p *Profile) { p.Proxy = px }); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"assigned": len(in.IDs), "proxies": len(list)})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	in.ChromePath = strings.Trim(strings.TrimSpace(in.ChromePath), `"`)
	if in.ChromePath != "" && strings.HasSuffix(strings.ToLower(in.ChromePath), ".lnk") {
		writeErr(w, 400, "Đây là file lối tắt (.lnk). Hãy chọn đúng file chrome.exe")
		return
	}
	if in.ChromePath != "" && !fileExists(in.ChromePath) {
		writeErr(w, 400, "Không tìm thấy file: "+in.ChromePath)
		return
	}
	if in.CacheMB < -1 || in.CacheMB > 5000 {
		writeErr(w, 400, "giới hạn bộ nhớ đệm không hợp lệ")
		return
	}
	switch in.ChromeLang {
	case "", "vi", "en", "system":
	default:
		writeErr(w, 400, "ngôn ngữ không hợp lệ")
		return
	}
	if err := s.store.SetSettings(in); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------- sync ----------

func (s *Server) syncLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server   string `json:"server"`
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
		Register bool   `json:"register"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	if err := s.sync.Login(in.Server, in.Username, in.Password, in.Code, in.Register); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	err := s.sync.SyncOnce()
	res := map[string]any{"ok": true, "profiles": len(s.store.Profiles())}
	if err != nil {
		res["warning"] = err.Error()
	}
	writeJSON(w, res)
}

func (s *Server) syncLogout(w http.ResponseWriter, r *http.Request) {
	s.mgr.Shutdown() // save cookies of open profiles first
	s.sync.Logout()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	for _, id := range s.mgr.RunningIDs() {
		_ = s.mgr.Capture(id)
	}
	if err := s.sync.SyncOnce(); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) syncUploadAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.mgr.UploadAll()
	res := map[string]any{"uploaded": n}
	if err != nil {
		res["warning"] = err.Error()
	}
	writeJSON(w, res)
}

func (s *Server) syncExport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	data, n, err := s.sync.Export(in.Password)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir := downloadsDir()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "QuanLyChrome-saoluu-"+time.Now().Format("20060102-1504")+".qlcbak")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": path, "dir": dir, "profiles": n})
}

func (s *Server) syncImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Data     []byte `json:"data"` // base64 in JSON
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 600<<20)).Decode(&in); err != nil {
		writeErr(w, 400, "file quá lớn hoặc không hợp lệ")
		return
	}
	n, err := s.sync.Import(in.Data, in.Password)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"profiles": n})
}

func (s *Server) syncPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	if err := s.sync.ChangePassword(in.Old, in.New); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) openPath(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil || in.Path == "" {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	// Only folders the app itself created/reported.
	if in.Path != downloadsDir() && in.Path != s.store.dataDir {
		writeErr(w, 403, "không được phép")
		return
	}
	_ = openFolder(in.Path)
	writeJSON(w, map[string]any{"ok": true})
}

func downloadsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

// vpnPass forwards Proxy VPN requests to the sync server on the VPS.
func (s *Server) vpnPass(w http.ResponseWriter, r *http.Request) {
	if !s.sync.Enabled() {
		writeErr(w, 400, "Hãy đăng nhập Đồng bộ trước — Proxy VPN chạy trên máy chủ đồng bộ (VPS) của bạn")
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<16))
	}
	if len(body) == 0 && r.Method != http.MethodGet {
		body = []byte("{}")
	}
	if r.Method == http.MethodGet {
		body = nil
	}
	out, _, err := s.sync.do(r.Method, r.URL.Path, body, nil)
	if err != nil {
		code := 502
		var ae *apiError
		if errors.As(err, &ae) {
			code = ae.Status
		}
		writeErr(w, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(out)
}

func (s *Server) clone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Count     int    `json:"count"`
		Group     string `json:"group"`
		CopyProxy bool   `json:"copyProxy"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, 400, "Hãy nhập tên")
		return
	}
	if in.Count < 1 {
		in.Count = 1
	}
	if in.Count > 50 {
		writeErr(w, 400, "Mỗi lần nhân bản tối đa 50 hồ sơ")
		return
	}
	names := []string{in.Name}
	if in.Count > 1 {
		names = names[:0]
		for i := 1; i <= in.Count; i++ {
			names = append(names, fmt.Sprintf("%s %d", in.Name, i))
		}
	}
	made, err := s.mgr.Clone(in.ID, names, strings.TrimSpace(in.Group), in.CopyProxy)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"created": len(made)})
}

func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	var in idsInput
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	var freed int64
	done, skipped := 0, 0
	for _, id := range in.IDs {
		if _, ok := s.store.Get(id); !ok {
			continue
		}
		if s.mgr.IsRunning(id) {
			skipped++
			continue
		}
		freed += ClearCache(s.store.ProfileDir(id))
		profileSizes.Forget(id)
		done++
	}
	writeJSON(w, map[string]any{"cleared": done, "skipped": skipped, "freed": freed})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var in idsInput
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	if err := s.store.SetTrashed(in.IDs, false); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"restored": len(in.IDs)})
}

func (s *Server) pin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs    []string `json:"ids"`
		Pinned bool     `json:"pinned"`
	}
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "Chưa chọn hồ sơ")
		return
	}
	for _, id := range in.IDs {
		if _, err := s.store.Update(id, func(p *Profile) { p.Pinned = in.Pinned }); err != nil {
			writeErr(w, 404, err.Error())
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) saveMeta(w http.ResponseWriter, r *http.Request) {
	var in SharedMeta
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "dữ liệu không hợp lệ")
		return
	}
	if _, err := time.Parse("2006-01-02", in.VPSExpiry); err != nil {
		writeErr(w, 400, "Ngày không hợp lệ")
		return
	}
	if err := s.store.SetMeta(in); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) totp(w http.ResponseWriter, r *http.Request) {
	p, ok := s.store.Get(r.PathValue("id"))
	if !ok || p.Account == nil {
		writeErr(w, 404, "hồ sơ chưa có mã 2FA")
		return
	}
	code, left, err := totpCode(p.Account.TwoFA, time.Now())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"code": code, "remaining": left})
}

func (s *Server) checkAll(w http.ResponseWriter, r *http.Request) {
	go s.mgr.CheckAll()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request) {
	var in idsInput
	_ = readJSON(r, &in)
	n, err := s.mgr.Tile(in.IDs)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"arranged": n})
}
