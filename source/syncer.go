package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const syncInterval = 20 * time.Second

type cookieState struct {
	Version int64  `json:"version"` // server version this computer has
	Hash    string `json:"hash"`    // hash of what we last uploaded/restored
}

type syncConfig struct {
	Server          string                  `json:"server"`
	Username        string                  `json:"username"`
	Token           string                  `json:"token,omitempty"`
	KeyProt         string                  `json:"keyProt,omitempty"`
	DeviceID        string                  `json:"deviceId"`
	ProfilesVersion int64                   `json:"profilesVersion"`
	ProfilesHash    string                  `json:"profilesHash"`
	Cookies         map[string]*cookieState `json:"cookies"`
	Site            map[string]*cookieState `json:"site"`
}

type blobMeta struct {
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	Size      int64     `json:"size"`
	Device    string    `json:"device"`
}

type lockInfo struct {
	Device     string    `json:"device"`
	DeviceName string    `json:"deviceName"`
	Since      time.Time `json:"since"`
	Expires    time.Time `json:"expires"`
}

// CookieBundle is what gets stored per profile (encrypted) on the server.
type CookieBundle struct {
	Cookies    cookieList `json:"cookies"`
	Bookmarks  []byte     `json:"bookmarks,omitempty"`
	CapturedAt time.Time  `json:"capturedAt"`
	Device     string     `json:"device"`
	DeviceID   string     `json:"deviceId,omitempty"`
}

func (b *CookieBundle) hash() string {
	return hashJSON(struct {
		C cookieList
		B []byte
	}{b.Cookies, b.Bookmarks})
}

type LockedError struct{ Lock lockInfo }

func (e *LockedError) Error() string {
	name := e.Lock.DeviceName
	if name == "" {
		name = "máy khác"
	}
	return fmt.Sprintf("Hồ sơ đang mở trên máy \"%s\" (từ %s)", name, e.Lock.Since.Local().Format("15:04 02/01"))
}

type Syncer struct {
	store      *Store
	path       string
	pendingDir string
	deviceName string
	hc         *http.Client

	mu       sync.Mutex
	cfg      syncConfig
	key      []byte
	lastSync time.Time
	lastErr  string
	busyText string
	capErr   string
	meta     map[string]blobMeta
	locks    map[string]lockInfo

	runMu   sync.Mutex // one sync pass at a time
	kick    chan struct{}
	running func() []string    // profile IDs open on this computer
	removed func(ids []string) // profiles deleted from another computer
}

func NewSyncer(store *Store, dataDir string) *Syncer {
	s := &Syncer{
		store:      store,
		path:       filepath.Join(dataDir, "sync.json"),
		pendingDir: filepath.Join(dataDir, "sync-pending"),
		hc:         &http.Client{Timeout: 60 * time.Second},
		meta:       map[string]blobMeta{},
		locks:      map[string]lockInfo{},
		kick:       make(chan struct{}, 1),
		running:    func() []string { return nil },
		removed:    func([]string) {},
	}
	s.deviceName, _ = os.Hostname()
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.cfg)
	}
	if s.cfg.DeviceID == "" {
		s.cfg.DeviceID = newID() + newID()
	}
	if s.cfg.Cookies == nil {
		s.cfg.Cookies = map[string]*cookieState{}
	}
	if s.cfg.Site == nil {
		s.cfg.Site = map[string]*cookieState{}
	}
	if s.cfg.KeyProt != "" && s.cfg.Token != "" {
		if k, err := unprotectKey(s.cfg.KeyProt); err == nil && len(k) == 32 {
			s.key = k
		} else {
			s.cfg.Token = "" // key unreadable (e.g. copied from another PC): log in again
		}
	}
	_ = s.saveLocked()
	store.onChange = s.Kick
	go s.loop()
	return s
}

func (s *Syncer) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Syncer) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key != nil && s.cfg.Token != ""
}

func (s *Syncer) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Syncer) loop() {
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		if s.Enabled() {
			s.SyncOnce()
		}
		select {
		case <-t.C:
		case <-s.kick:
			time.Sleep(1500 * time.Millisecond) // debounce bursts of edits
		}
	}
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func normalizeServer(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if u != "" && !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return u
}

// ---------- HTTP ----------

type apiError struct {
	Status int
	Msg    string
	Body   []byte
}

func (e *apiError) Error() string { return e.Msg }

func (s *Syncer) do(method, path string, body []byte, hdr map[string]string) ([]byte, http.Header, error) {
	s.mu.Lock()
	server, token, dev := s.cfg.Server, s.cfg.Token, s.cfg.DeviceID
	s.mu.Unlock()
	return s.doWith(server, token, dev, method, path, body, hdr)
}

func (s *Syncer) doWith(server, token, dev, method, path string, body []byte, hdr map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequest(method, server+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Device", dev)
	if body != nil && hdr["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("không kết nối được máy chủ đồng bộ: %s", friendlyNetErr(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("máy chủ đồng bộ trả lỗi %d", resp.StatusCode)
		}
		if resp.StatusCode == 401 && token != "" {
			s.mu.Lock()
			if s.cfg.Token == token {
				s.cfg.Token, s.key = "", nil
				_ = s.saveLocked()
			}
			s.mu.Unlock()
		}
		return nil, nil, &apiError{Status: resp.StatusCode, Msg: e.Error, Body: b}
	}
	return b, resp.Header, nil
}

// ---------- account ----------

func (s *Syncer) Login(server, username, password, code string, register bool) error {
	server = normalizeServer(server)
	username = strings.ToLower(strings.TrimSpace(username))
	if server == "" || username == "" || password == "" {
		return errors.New("Hãy nhập đủ máy chủ, tên đăng nhập và mật khẩu")
	}
	if register && len(password) < 8 {
		return errors.New("Mật khẩu cần ít nhất 8 ký tự")
	}
	authKey, wrapKey := deriveKeys(username, password)
	in := map[string]string{
		"username": username,
		"authKey":  base64.StdEncoding.EncodeToString(authKey),
		"device":   s.deviceName,
		"code":     code,
	}
	var master []byte
	path := "/api/login"
	if register {
		master = make([]byte, 32)
		_, _ = rand.Read(master)
		wrapped, err := seal(wrapKey, master)
		if err != nil {
			return err
		}
		in["wrappedKey"] = base64.StdEncoding.EncodeToString(wrapped)
		path = "/api/register"
	}
	body, _ := json.Marshal(in)
	s.mu.Lock()
	dev := s.cfg.DeviceID
	s.mu.Unlock()
	resp, _, err := s.doWith(server, "", dev, "POST", path, body, nil)
	if err != nil {
		return err
	}
	var out struct {
		Token      string `json:"token"`
		WrappedKey string `json:"wrappedKey"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || out.Token == "" {
		return errors.New("máy chủ trả dữ liệu không hợp lệ (địa chỉ máy chủ có đúng không?)")
	}
	if !register {
		wrapped, err := base64.StdEncoding.DecodeString(out.WrappedKey)
		if err != nil {
			return errDecrypt
		}
		if master, err = unseal(wrapKey, wrapped); err != nil || len(master) != 32 {
			return errors.New("không mở được khóa dữ liệu của tài khoản")
		}
	}
	prot, err := protectKey(master)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.cfg.Server != server || s.cfg.Username != username {
		s.cfg.ProfilesVersion, s.cfg.ProfilesHash = 0, ""
		s.cfg.Cookies = map[string]*cookieState{}
	}
	s.cfg.Server, s.cfg.Username, s.cfg.Token, s.cfg.KeyProt = server, username, out.Token, prot
	s.key = master
	s.lastErr = ""
	err = s.saveLocked()
	s.mu.Unlock()
	s.Kick()
	return err
}

func (s *Syncer) Logout() {
	if s.Enabled() {
		_, _, _ = s.do("POST", "/api/logout", []byte("{}"), nil)
	}
	s.mu.Lock()
	s.cfg.Token, s.cfg.KeyProt, s.key = "", "", nil
	s.lastErr = ""
	_ = s.saveLocked()
	s.mu.Unlock()
}

func (s *Syncer) ChangePassword(oldPass, newPass string) error {
	if !s.Enabled() {
		return errors.New("chưa đăng nhập đồng bộ")
	}
	if len(newPass) < 8 {
		return errors.New("Mật khẩu mới cần ít nhất 8 ký tự")
	}
	s.mu.Lock()
	user, master := s.cfg.Username, append([]byte(nil), s.key...)
	s.mu.Unlock()
	oldAuth, _ := deriveKeys(user, oldPass)
	newAuth, newWrap := deriveKeys(user, newPass)
	wrapped, err := seal(newWrap, master)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{
		"username":   user,
		"authKey":    base64.StdEncoding.EncodeToString(oldAuth),
		"newAuthKey": base64.StdEncoding.EncodeToString(newAuth),
		"wrappedKey": base64.StdEncoding.EncodeToString(wrapped),
	})
	_, _, err = s.do("POST", "/api/password", body, nil)
	return err
}

// ---------- status ----------

type SyncStatus struct {
	Enabled    bool                `json:"enabled"`
	Server     string              `json:"server"`
	Username   string              `json:"username"`
	DeviceName string              `json:"deviceName"`
	LastSync   *time.Time          `json:"lastSync,omitempty"`
	Error      string              `json:"error,omitempty"`
	Busy       string              `json:"busy,omitempty"`
	DataDir    string              `json:"dataDir"`
	Locks      map[string]lockInfo `json:"locks"` // held by OTHER computers
	Cloud      map[string]blobMeta `json:"cloud"` // profile id -> cookie blob
}

func (s *Syncer) Status() SyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SyncStatus{
		Enabled: s.key != nil && s.cfg.Token != "", Server: s.cfg.Server, Username: s.cfg.Username,
		DeviceName: s.deviceName, Error: s.lastErr, Busy: s.busyText,
		DataDir: filepath.Dir(s.path),
		Locks:   map[string]lockInfo{}, Cloud: map[string]blobMeta{},
	}
	if st.Error == "" {
		st.Error = s.capErr
	}
	if !s.lastSync.IsZero() {
		t := s.lastSync
		st.LastSync = &t
	}
	if !st.Enabled {
		return st
	}
	now := time.Now()
	for id, l := range s.locks {
		if l.Device != s.cfg.DeviceID && now.Before(l.Expires) {
			st.Locks[id] = l
		}
	}
	for k, m := range s.meta {
		if strings.HasPrefix(k, "c_") {
			st.Cloud[k[2:]] = m
		}
	}
	return st
}

func (s *Syncer) setCaptureErr(e string) {
	s.mu.Lock()
	s.capErr = e
	s.mu.Unlock()
}

func (s *Syncer) setBusy(t string) {
	s.mu.Lock()
	s.busyText = t
	s.mu.Unlock()
}

// ---------- main sync pass ----------

func (s *Syncer) SyncOnce() error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	err := s.syncPass()
	s.mu.Lock()
	if err != nil {
		s.lastErr = err.Error()
		log.Printf("LỖI đồng bộ: %v", err)
	} else {
		s.lastErr = ""
		s.lastSync = time.Now()
	}
	s.mu.Unlock()
	return err
}

func (s *Syncer) refreshMeta() error {
	b, _, err := s.do("GET", "/api/blobs", nil, nil)
	if err != nil {
		return err
	}
	meta := map[string]blobMeta{}
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	s.mu.Lock()
	s.meta = meta
	s.mu.Unlock()
	return nil
}

func (s *Syncer) syncPass() error {
	if !s.Enabled() {
		return nil
	}
	if err := s.refreshMeta(); err != nil {
		return err
	}
	// Keep our locks alive and learn about the other computers' locks.
	for _, id := range s.running() {
		_ = s.TakeLock(id, false)
	}
	if b, _, err := s.do("GET", "/api/locks", nil, nil); err == nil {
		locks := map[string]lockInfo{}
		if json.Unmarshal(b, &locks) == nil {
			s.mu.Lock()
			s.locks = locks
			s.mu.Unlock()
		}
	}

	// Profiles list: pull + merge, then push if we have changes.
	for attempt := 0; attempt < 3; attempt++ {
		s.mu.Lock()
		rm, haveRemote := s.meta["profiles"]
		localVer := s.cfg.ProfilesVersion
		key := s.key
		s.mu.Unlock()
		if key == nil {
			return nil
		}
		if haveRemote && rm.Version != localVer {
			data, _, err := s.do("GET", "/api/blobs/profiles", nil, nil)
			if err != nil {
				return err
			}
			var doc SyncDoc
			if err := unsealJSON(key, data, &doc); err != nil {
				return fmt.Errorf("không giải mã được danh sách hồ sơ: %w", err)
			}
			removed, err := s.store.Merge(doc)
			if err != nil {
				return err
			}
			if len(removed) > 0 {
				s.removed(removed)
			}
			s.mu.Lock()
			s.cfg.ProfilesVersion, s.cfg.ProfilesHash = rm.Version, hashJSON(doc)
			_ = s.saveLocked()
			s.mu.Unlock()
		}
		local := s.store.SyncDoc()
		h := hashJSON(local)
		s.mu.Lock()
		same := h == s.cfg.ProfilesHash && (haveRemote || len(local.Profiles) == 0)
		base := s.cfg.ProfilesVersion
		s.mu.Unlock()
		if same {
			break
		}
		data, err := sealJSON(key, local)
		if err != nil {
			return err
		}
		b, _, err := s.do("PUT", "/api/blobs/profiles", data, map[string]string{
			"Content-Type": "application/octet-stream", "X-Base-Version": strconv.FormatInt(base, 10)})
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 409 {
			_ = s.refreshMeta() // someone else pushed first: merge again
			continue
		}
		if err != nil {
			return err
		}
		var out struct{ Version int64 }
		_ = json.Unmarshal(b, &out)
		s.mu.Lock()
		s.cfg.ProfilesVersion, s.cfg.ProfilesHash = out.Version, h
		s.meta["profiles"] = blobMeta{Version: out.Version, UpdatedAt: time.Now()}
		_ = s.saveLocked()
		s.mu.Unlock()
		break
	}

	// Remove cookie data of deleted profiles from the server.
	deleted := s.store.SyncDoc().Deleted
	s.mu.Lock()
	var gone []string
	for k := range s.meta {
		if strings.HasPrefix(k, "c_") || strings.HasPrefix(k, "s_") {
			if _, ok := deleted[k[2:]]; ok {
				gone = append(gone, k)
			}
		}
	}
	s.mu.Unlock()
	for _, k := range gone {
		if _, _, err := s.do("DELETE", "/api/blobs/"+k, nil, nil); err == nil {
			s.mu.Lock()
			delete(s.meta, k)
			delete(s.cfg.Cookies, k[2:])
			_ = s.saveLocked()
			s.mu.Unlock()
		}
	}

	return s.flushPending()
}

// ---------- locks ----------

func (s *Syncer) TakeLock(id string, force bool) error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	dev := s.cfg.DeviceID
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"device": dev, "deviceName": s.deviceName, "force": force})
	_, _, err := s.do("POST", "/api/locks/"+id, body, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 409 {
		var r struct {
			Lock lockInfo `json:"lock"`
		}
		_ = json.Unmarshal(ae.Body, &r)
		return &LockedError{Lock: r.Lock}
	}
	return err
}

func (s *Syncer) ReleaseLock(id string) {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	dev := s.cfg.DeviceID
	s.mu.Unlock()
	_, _, _ = s.do("DELETE", "/api/locks/"+id+"?device="+dev, nil, nil)
}

// ---------- cookies ----------

// CookiesToRestore returns the server's cookie bundle for a profile if it is
// newer than what this computer has.
func (s *Syncer) CookiesToRestore(id string) (*CookieBundle, int64, error) {
	if !s.Enabled() {
		return nil, 0, nil
	}
	if err := s.refreshMeta(); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	m, ok := s.meta["c_"+id]
	var have int64
	if cs := s.cfg.Cookies[id]; cs != nil {
		have = cs.Version
	}
	key := s.key
	s.mu.Unlock()
	if !ok || m.Version <= have {
		return nil, 0, nil
	}
	data, hdr, err := s.do("GET", "/api/blobs/c_"+id, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	var b CookieBundle
	if err := unsealJSON(key, data, &b); err != nil {
		return nil, 0, err
	}
	v, _ := strconv.ParseInt(hdr.Get("X-Version"), 10, 64)
	return &b, v, nil
}

// MarkRestored records that the bundle at version v is now in local Chrome.
func (s *Syncer) DeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.DeviceID
}

func (s *Syncer) MarkRestored(id string, v int64, b *CookieBundle) {
	s.mu.Lock()
	s.cfg.Cookies[id] = &cookieState{Version: v, Hash: b.hash()}
	_ = s.saveLocked()
	s.mu.Unlock()
}

// UploadCookies uploads a bundle if it changed. On network errors the
// encrypted bundle is kept on disk and retried later.
func (s *Syncer) UploadCookies(id string, b *CookieBundle, force bool) error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	if s.store.Settings().AllowMultiOpen && !force {
		// Other computers may have this profile open too: merge their newer
		// cookies instead of overwriting them.
		if remote, _, err := s.CookiesToRestore(id); err == nil && remote != nil {
			b.Cookies = mergeCookies(remote.Cookies, b.Cookies)
		}
	}
	h := b.hash()
	s.mu.Lock()
	cs := s.cfg.Cookies[id]
	s.mu.Unlock()
	if !force && cs != nil && cs.Hash == h {
		return nil
	}
	b.Device = s.deviceName
	s.mu.Lock()
	b.DeviceID = s.cfg.DeviceID
	s.mu.Unlock()
	data, err := sealJSON(key, b)
	if err != nil {
		return err
	}
	if err := s.putCookies(id, data, h); err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			_ = os.MkdirAll(s.pendingDir, 0o700)
			_ = os.WriteFile(filepath.Join(s.pendingDir, id+".bin"), data, 0o600)
			_ = os.WriteFile(filepath.Join(s.pendingDir, id+".hash"), []byte(h), 0o600)
		}
		return err
	}
	return nil
}

func (s *Syncer) putCookies(id string, data []byte, h string) error {
	b, _, err := s.do("PUT", "/api/blobs/c_"+id, data, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	var out struct{ Version int64 }
	_ = json.Unmarshal(b, &out)
	s.mu.Lock()
	s.cfg.Cookies[id] = &cookieState{Version: out.Version, Hash: h}
	s.meta["c_"+id] = blobMeta{Version: out.Version, UpdatedAt: time.Now(), Size: int64(len(data))}
	_ = s.saveLocked()
	s.mu.Unlock()
	_ = os.Remove(filepath.Join(s.pendingDir, id+".bin"))
	_ = os.Remove(filepath.Join(s.pendingDir, id+".hash"))
	return nil
}

func (s *Syncer) flushPending() error {
	entries, _ := os.ReadDir(s.pendingDir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".bin")
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.pendingDir, e.Name()))
		if err != nil {
			continue
		}
		h, _ := os.ReadFile(filepath.Join(s.pendingDir, id+".hash"))
		if err := s.putCookies(id, data, string(h)); err != nil {
			return err
		}
	}
	return nil
}

// ---------- backup ----------

type backupFile struct {
	Created  time.Time                `json:"created"`
	Device   string                   `json:"device"`
	Profiles SyncDoc                  `json:"profiles"`
	Cookies  map[string]*CookieBundle `json:"cookies"`
}

func (s *Syncer) Export(password string) ([]byte, int, error) {
	if !s.Enabled() {
		return nil, 0, errors.New("Hãy đăng nhập đồng bộ trước")
	}
	if len(password) < 8 {
		return nil, 0, errors.New("Mật khẩu bản sao lưu cần ít nhất 8 ký tự")
	}
	if err := s.SyncOnce(); err != nil {
		return nil, 0, err
	}
	bk := backupFile{Created: time.Now(), Device: s.deviceName, Profiles: s.store.SyncDoc(), Cookies: map[string]*CookieBundle{}}
	s.mu.Lock()
	key := s.key
	var ids []string
	for k := range s.meta {
		if strings.HasPrefix(k, "c_") {
			ids = append(ids, k[2:])
		}
	}
	s.mu.Unlock()
	for i, id := range ids {
		s.setBusy(fmt.Sprintf("Đang tạo bản sao lưu %d/%d…", i+1, len(ids)))
		data, _, err := s.do("GET", "/api/blobs/c_"+id, nil, nil)
		if err != nil {
			s.setBusy("")
			return nil, 0, err
		}
		var b CookieBundle
		if err := unsealJSON(key, data, &b); err == nil {
			bk.Cookies[id] = &b
		}
	}
	s.setBusy("")
	out, err := sealBackup(password, bk)
	return out, len(bk.Profiles.Profiles), err
}

func (s *Syncer) Import(data []byte, password string) (int, error) {
	if !s.Enabled() {
		return 0, errors.New("Hãy đăng nhập đồng bộ trước khi khôi phục")
	}
	var bk backupFile
	if err := openBackup(password, data, &bk); err != nil {
		return 0, err
	}
	// Restoring means the backup wins: make its profiles look newest.
	now := time.Now()
	for i := range bk.Profiles.Profiles {
		bk.Profiles.Profiles[i].UpdatedAt = now
	}
	bk.Profiles.Deleted = nil
	if _, err := s.store.Merge(bk.Profiles); err != nil {
		return 0, err
	}
	i := 0
	for id, b := range bk.Cookies {
		i++
		s.setBusy(fmt.Sprintf("Đang khôi phục %d/%d…", i, len(bk.Cookies)))
		if err := s.UploadCookies(id, b, true); err != nil {
			s.setBusy("")
			return 0, err
		}
		// Force a restore into local Chrome on next open.
		s.mu.Lock()
		if cs := s.cfg.Cookies[id]; cs != nil {
			cs.Version, cs.Hash = 0, ""
		}
		_ = s.saveLocked()
		s.mu.Unlock()
	}
	s.setBusy("")
	s.Kick()
	return len(bk.Profiles.Profiles), nil
}

func cookieKey(c map[string]any) string {
	return fmt.Sprint(c["domain"], "|", c["path"], "|", c["name"], "|", c["partitionKey"])
}

// mergeCookies returns base overlaid with top (top wins on the same cookie).
func mergeCookies(base, top cookieList) cookieList {
	idx := map[string]int{}
	out := make(cookieList, 0, len(base)+len(top))
	for _, c := range base {
		idx[cookieKey(c)] = len(out)
		out = append(out, c)
	}
	for _, c := range top {
		if i, ok := idx[cookieKey(c)]; ok {
			out[i] = c
		} else {
			idx[cookieKey(c)] = len(out)
			out = append(out, c)
		}
	}
	return out
}

// ---------- site data (Local Storage / IndexedDB) ----------

// UploadSite stores a packed site-data archive for a profile if it changed.
func (s *Syncer) UploadSite(id string, pack []byte) error {
	if !s.Enabled() {
		return nil
	}
	h := sha256.Sum256(pack)
	hs := hex.EncodeToString(h[:])
	s.mu.Lock()
	cs := s.cfg.Site[id]
	key := s.key
	s.mu.Unlock()
	if cs != nil && cs.Hash == hs {
		return nil
	}
	data, err := seal(key, pack)
	if err != nil {
		return err
	}
	b, _, err := s.do("PUT", "/api/blobs/s_"+id, data, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	var out struct{ Version int64 }
	_ = json.Unmarshal(b, &out)
	s.mu.Lock()
	s.cfg.Site[id] = &cookieState{Version: out.Version, Hash: hs}
	s.meta["s_"+id] = blobMeta{Version: out.Version, UpdatedAt: time.Now(), Size: int64(len(data)), Device: s.cfg.DeviceID}
	_ = s.saveLocked()
	s.mu.Unlock()
	return nil
}

// SiteToRestore returns the server's site-data archive if another computer
// saved a newer one than this computer has.
func (s *Syncer) SiteToRestore(id string) ([]byte, int64, error) {
	if !s.Enabled() {
		return nil, 0, nil
	}
	if err := s.refreshMeta(); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	m, ok := s.meta["s_"+id]
	var have int64
	if cs := s.cfg.Site[id]; cs != nil {
		have = cs.Version
	}
	key, me := s.key, s.cfg.DeviceID
	s.mu.Unlock()
	if !ok || m.Version <= have {
		return nil, 0, nil
	}
	if m.Device == me { // saved by this computer: local files are as new
		s.markSite(id, m.Version, "")
		return nil, 0, nil
	}
	data, hdr, err := s.do("GET", "/api/blobs/s_"+id, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	pack, err := unseal(key, data)
	if err != nil {
		return nil, 0, err
	}
	v, _ := strconv.ParseInt(hdr.Get("X-Version"), 10, 64)
	return pack, v, nil
}

func (s *Syncer) markSite(id string, v int64, hash string) {
	s.mu.Lock()
	s.cfg.Site[id] = &cookieState{Version: v, Hash: hash}
	_ = s.saveLocked()
	s.mu.Unlock()
}

// creds returns the server address, session token and device id.
func (s *Syncer) creds() (string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Server, s.cfg.Token, s.cfg.DeviceID
}
