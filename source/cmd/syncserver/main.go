// syncserver stores the encrypted data of "Quản lý Chrome" so profiles can be
// opened on another computer. It never sees plaintext: clients encrypt with a
// key derived from the user's password before uploading.
//
// Everything lives under -data, so moving to another VPS = copy that folder.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxBlob        = 64 << 20 // 64 MB per blob
	sessionTTL     = 90 * 24 * time.Hour
	lockTTL        = 3 * time.Minute
	failWindow     = 10 * time.Minute
	maxFailsPerIP  = 15
	maxFailsPerUsr = 10
)

var (
	reUser = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)
	reKey  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

type user struct {
	AuthHash   string    `json:"authHash"`   // hex sha256(authKey); authKey is already an Argon2 output
	WrappedKey string    `json:"wrappedKey"` // master key encrypted by the client, opaque to us
	Created    time.Time `json:"created"`
	HubHash    string    `json:"hubHash,omitempty"` // bcrypt of the "Trung tâm Zalo" password
}

type session struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
	Device  string    `json:"device"`
}

type blobMeta struct {
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Size      int64     `json:"size"`
	Device    string    `json:"device,omitempty"`
}

type lock struct {
	Device     string    `json:"device"`
	DeviceName string    `json:"deviceName"`
	Since      time.Time `json:"since"`
	Expires    time.Time `json:"expires"`
}

type server struct {
	dir  string
	code string
	dl   string // folder with the app download page, served at /tai/

	proxydURL string // local proxy helper (ExpressVPN proxies), "" = disabled
	proxydKey string

	mu       sync.Mutex
	users    map[string]*user
	sessions map[string]*session // key = hex sha256(token)
	index    map[string]map[string]*blobMeta
	locks    map[string]map[string]*lock
	fails    map[string][]time.Time
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "listen address")
	dir := flag.String("data", "./data", "data directory")
	code := flag.String("code", os.Getenv("SYNC_REGISTER_CODE"), "registration code (required to create accounts)")
	dl := flag.String("downloads", "", "folder served at /tai/ (app download page)")
	pd := flag.String("proxyd", "", "URL of the local proxy helper, e.g. http://127.0.0.1:8791")
	pdKey := flag.String("proxyd-key", "", "file with the proxy helper key")
	flag.Parse()

	s := &server{dir: *dir, code: *code, dl: *dl, proxydURL: *pd,
		users: map[string]*user{}, sessions: map[string]*session{},
		index: map[string]map[string]*blobMeta{}, locks: map[string]map[string]*lock{}, fails: map[string][]time.Time{}}
	if *pdKey != "" {
		if b, err := os.ReadFile(*pdKey); err == nil {
			s.proxydKey = strings.TrimSpace(string(b))
		}
	}
	if err := s.load(); err != nil {
		log.Fatal(err)
	}
	if s.code == "" {
		log.Println("WARNING: no -code set, registration is disabled")
	}
	log.Printf("listening on %s, data in %s", *addr, *dir)
	srv := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 20 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// ---------- persistence ----------

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

func userDir(dir, u string) string {
	h := sha256.Sum256([]byte(u))
	return filepath.Join(dir, "blobs", hex.EncodeToString(h[:8]))
}

func (s *server) load() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(s.dir, "users.json"), &s.users); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(s.dir, "sessions.json"), &s.sessions); err != nil {
		return err
	}
	for u := range s.users {
		idx := map[string]*blobMeta{}
		if err := readJSON(filepath.Join(userDir(s.dir, u), "index.json"), &idx); err != nil {
			return err
		}
		s.index[u] = idx
	}
	return nil
}

func (s *server) saveUsersLocked() error {
	return saveJSON(filepath.Join(s.dir, "users.json"), s.users)
}
func (s *server) saveSessionsLocked() error {
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.Expires) {
			delete(s.sessions, k)
		}
	}
	return saveJSON(filepath.Join(s.dir, "sessions.json"), s.sessions)
}
func (s *server) saveIndexLocked(u string) error {
	return saveJSON(filepath.Join(userDir(s.dir, u), "index.json"), s.index[u])
}

// ---------- http helpers ----------

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	// Behind nginx on the same machine: trust its X-Real-IP header.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if x := r.Header.Get("X-Real-IP"); x != "" {
			return x
		}
	}
	return host
}

func (s *server) tooManyFails(keys ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-failWindow)
	for i, k := range keys {
		var kept []time.Time
		for _, t := range s.fails[k] {
			if t.After(cut) {
				kept = append(kept, t)
			}
		}
		s.fails[k] = kept
		limit := maxFailsPerIP
		if i > 0 {
			limit = maxFailsPerUsr
		}
		if len(kept) >= limit {
			return true
		}
	}
	return false
}

func (s *server) recordFail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.fails[k] = append(s.fails[k], time.Now())
	}
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- routes ----------

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	if s.dl != "" {
		mux.Handle("GET /tai/", http.StripPrefix("/tai/", noCache(http.FileServer(http.Dir(s.dl)))))
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/tai/", http.StatusFound)
		})
	}
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "app": "chrome-sync", "registration": s.code != ""})
	})
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.auth(s.logout))
	mux.HandleFunc("POST /api/password", s.auth(s.changePassword))
	mux.HandleFunc("GET /api/blobs", s.auth(s.listBlobs))
	mux.HandleFunc("GET /api/blobs/{key}", s.auth(s.getBlob))
	mux.HandleFunc("PUT /api/blobs/{key}", s.auth(s.putBlob))
	mux.HandleFunc("DELETE /api/blobs/{key}", s.auth(s.deleteBlob))
	mux.HandleFunc("GET /api/locks", s.auth(s.listLocks))
	mux.HandleFunc("POST /api/locks/{key}", s.auth(s.takeLock))
	mux.HandleFunc("DELETE /api/locks/{key}", s.auth(s.releaseLock))
	mux.HandleFunc("/api/vpn/", s.auth(s.vpn))
	s.hubRoutes(mux)
	return mux
}

type authed func(w http.ResponseWriter, r *http.Request, u string, sess *session)

func (s *server) auth(h authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			fail(w, 401, "Chưa đăng nhập")
			return
		}
		s.mu.Lock()
		sess := s.sessions[hashHex([]byte(tok))]
		var u string
		if sess != nil && time.Now().Before(sess.Expires) && s.users[sess.User] != nil {
			u = sess.User
		}
		s.mu.Unlock()
		if u == "" {
			fail(w, 401, "Phiên đăng nhập đã hết hạn, hãy đăng nhập lại")
			return
		}
		h(w, r, u, sess)
	}
}

type credIn struct {
	Username   string `json:"username"`
	AuthKey    string `json:"authKey"`    // base64
	WrappedKey string `json:"wrappedKey"` // base64, register / password change only
	Code       string `json:"code"`
	Device     string `json:"device"`
	NewAuthKey string `json:"newAuthKey"`
}

func decodeCred(r *http.Request) (credIn, []byte, error) {
	var in credIn
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		return in, nil, errors.New("dữ liệu không hợp lệ")
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	ak, err := base64.StdEncoding.DecodeString(in.AuthKey)
	if err != nil || len(ak) != 32 {
		return in, nil, errors.New("dữ liệu không hợp lệ")
	}
	return in, ak, nil
}

func (s *server) newSessionLocked(u, device string) (string, error) {
	tok := newToken()
	s.sessions[hashHex([]byte(tok))] = &session{User: u, Expires: time.Now().Add(sessionTTL), Device: device}
	return tok, s.saveSessionsLocked()
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.tooManyFails("ip:" + ip) {
		fail(w, 429, "Thử sai quá nhiều lần, hãy đợi 10 phút")
		return
	}
	in, ak, err := decodeCred(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if s.code == "" || subtle.ConstantTimeCompare([]byte(in.Code), []byte(s.code)) != 1 {
		s.recordFail("ip:" + ip)
		fail(w, 403, "Mã đăng ký không đúng")
		return
	}
	if !reUser.MatchString(in.Username) {
		fail(w, 400, "Tên đăng nhập 3–32 ký tự: chữ thường không dấu, số, dấu . _ -")
		return
	}
	if wk, err := base64.StdEncoding.DecodeString(in.WrappedKey); err != nil || len(wk) < 32 || len(wk) > 256 {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users[in.Username] != nil {
		fail(w, 409, "Tên đăng nhập đã tồn tại")
		return
	}
	s.users[in.Username] = &user{AuthHash: hashHex(ak), WrappedKey: in.WrappedKey, Created: time.Now()}
	s.index[in.Username] = map[string]*blobMeta{}
	if err := s.saveUsersLocked(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	tok, err := s.newSessionLocked(in.Username, in.Device)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	log.Printf("registered user %s from %s", in.Username, ip)
	reply(w, 200, map[string]string{"token": tok, "wrappedKey": in.WrappedKey})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	in, ak, err := decodeCred(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if s.tooManyFails("ip:"+ip, "user:"+in.Username) {
		fail(w, 429, "Đăng nhập sai quá nhiều lần, hãy đợi 10 phút")
		return
	}
	s.mu.Lock()
	u := s.users[in.Username]
	ok := u != nil && subtle.ConstantTimeCompare([]byte(hashHex(ak)), []byte(u.AuthHash)) == 1
	s.mu.Unlock()
	if !ok {
		s.recordFail("ip:"+ip, "user:"+in.Username)
		fail(w, 401, "Sai tên đăng nhập hoặc mật khẩu")
		return
	}
	s.mu.Lock()
	tok, err := s.newSessionLocked(in.Username, in.Device)
	wk := u.WrappedKey
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]string{"token": tok, "wrappedKey": wk})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	delete(s.sessions, hashHex([]byte(tok)))
	_ = s.saveSessionsLocked()
	s.mu.Unlock()
	reply(w, 200, map[string]bool{"ok": true})
}

// changePassword: client re-wraps the same master key with the new password,
// so no data has to be re-encrypted.
func (s *server) changePassword(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	in, ak, err := decodeCred(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	nk, err := base64.StdEncoding.DecodeString(in.NewAuthKey)
	if err != nil || len(nk) != 32 {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	if wk, err := base64.StdEncoding.DecodeString(in.WrappedKey); err != nil || len(wk) < 32 || len(wk) > 256 {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	usr := s.users[u]
	if subtle.ConstantTimeCompare([]byte(hashHex(ak)), []byte(usr.AuthHash)) != 1 {
		fail(w, 401, "Mật khẩu hiện tại không đúng")
		return
	}
	usr.AuthHash, usr.WrappedKey = hashHex(nk), in.WrappedKey
	if err := s.saveUsersLocked(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) listBlobs(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	s.mu.Lock()
	out := map[string]blobMeta{}
	for k, m := range s.index[u] {
		out[k] = *m
	}
	s.mu.Unlock()
	reply(w, 200, out)
}

func (s *server) getBlob(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	key := r.PathValue("key")
	if !reKey.MatchString(key) {
		fail(w, 400, "khóa không hợp lệ")
		return
	}
	s.mu.Lock()
	m := s.index[u][key]
	var meta blobMeta
	if m != nil {
		meta = *m
	}
	s.mu.Unlock()
	if m == nil {
		fail(w, 404, "không có dữ liệu")
		return
	}
	f, err := os.Open(filepath.Join(userDir(s.dir, u), key+".bin"))
	if err != nil {
		fail(w, 404, "không có dữ liệu")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Version", strconv.FormatInt(meta.Version, 10))
	_, _ = io.Copy(w, f)
}

func (s *server) putBlob(w http.ResponseWriter, r *http.Request, u string, sess *session) {
	key := r.PathValue("key")
	if !reKey.MatchString(key) {
		fail(w, 400, "khóa không hợp lệ")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBlob))
	if err != nil {
		fail(w, 413, "dữ liệu quá lớn")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.index[u]
	cur := idx[key]
	// Optional optimistic concurrency: X-Base-Version must match.
	if bv := r.Header.Get("X-Base-Version"); bv != "" {
		base, _ := strconv.ParseInt(bv, 10, 64)
		var have int64
		if cur != nil {
			have = cur.Version
		}
		if base != have {
			reply(w, 409, map[string]any{"error": "dữ liệu trên máy chủ đã thay đổi", "version": have})
			return
		}
	}
	if err := writeAtomic(filepath.Join(userDir(s.dir, u), key+".bin"), body); err != nil {
		fail(w, 500, err.Error())
		return
	}
	nv := int64(1)
	if cur != nil {
		nv = cur.Version + 1
	}
	idx[key] = &blobMeta{Version: nv, UpdatedAt: time.Now(), Size: int64(len(body)), Device: r.Header.Get("X-Device")}
	if err := s.saveIndexLocked(u); err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]int64{"version": nv})
}

func (s *server) deleteBlob(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	key := r.PathValue("key")
	if !reKey.MatchString(key) {
		fail(w, 400, "khóa không hợp lệ")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index[u], key)
	_ = os.Remove(filepath.Join(userDir(s.dir, u), key+".bin"))
	if err := s.saveIndexLocked(u); err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) listLocks(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	s.mu.Lock()
	out := map[string]lock{}
	now := time.Now()
	for k, l := range s.locks[u] {
		if now.Before(l.Expires) {
			out[k] = *l
		}
	}
	s.mu.Unlock()
	reply(w, 200, out)
}

// takeLock marks a profile as open on one device (renewed every minute).
func (s *server) takeLock(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	key := r.PathValue("key")
	var in struct {
		Device     string `json:"device"`
		DeviceName string `json:"deviceName"`
		Force      bool   `json:"force"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || in.Device == "" || !reKey.MatchString(key) {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[u] == nil {
		s.locks[u] = map[string]*lock{}
	}
	now := time.Now()
	cur := s.locks[u][key]
	if cur != nil && now.Before(cur.Expires) && cur.Device != in.Device && !in.Force {
		reply(w, 409, map[string]any{"error": "đang mở trên máy khác", "lock": cur})
		return
	}
	since := now
	if cur != nil && cur.Device == in.Device && now.Before(cur.Expires) {
		since = cur.Since
	}
	s.locks[u][key] = &lock{Device: in.Device, DeviceName: in.DeviceName, Since: since, Expires: now.Add(lockTTL)}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) releaseLock(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	key := r.PathValue("key")
	dev := r.URL.Query().Get("device")
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.locks[u][key]; l != nil && l.Device == dev {
		delete(s.locks[u], key)
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// isAdminLocked: the first account created on this server owns the VPS
// features (ExpressVPN proxies).
func (s *server) isAdminLocked(u string) bool {
	var first string
	var t time.Time
	for name, usr := range s.users {
		if first == "" || usr.Created.Before(t) {
			first, t = name, usr.Created
		}
	}
	return u == first
}

var proxydClient = &http.Client{Timeout: 3 * time.Minute}

// vpn forwards /api/vpn/<path> to the local proxy helper.
func (s *server) vpn(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	if s.proxydURL == "" || s.proxydKey == "" {
		fail(w, 501, "Máy chủ chưa bật tính năng Proxy VPN — hãy cập nhật máy chủ (chạy lại install.sh)")
		return
	}
	s.mu.Lock()
	admin := s.isAdminLocked(u)
	s.mu.Unlock()
	if !admin {
		fail(w, 403, "Chỉ tài khoản chủ máy chủ mới dùng được Proxy VPN")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/vpn")
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.proxydURL+path, io.LimitReader(r.Body, 1<<16))
	if err != nil {
		fail(w, 400, "yêu cầu không hợp lệ")
		return
	}
	req.Header.Set("X-Proxyd-Key", s.proxydKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := proxydClient.Do(req)
	if err != nil {
		fail(w, 502, "Dịch vụ Proxy VPN trên VPS không phản hồi: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
