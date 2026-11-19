package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proxy describes the upstream proxy a profile goes through.
// Type is "" (no proxy), "http" or "socks5".
type Proxy struct {
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Pass string `json:"pass"`
}

func (p Proxy) Enabled() bool { return p.Type != "" && p.Host != "" && p.Port > 0 }
func (p Proxy) HasAuth() bool { return p.User != "" || p.Pass != "" }

type Profile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Group     string    `json:"group"`
	Notes     string    `json:"notes"`
	StartURL  string    `json:"startUrl"`
	Proxy     Proxy     `json:"proxy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Chrome Web Store extensions this profile should have. Opened for the
	// user to install the first time the profile is used on a computer.
	Extensions []string `json:"extensions,omitempty"`

	// Local-only fields (not synced between computers).
	LastOpened *time.Time `json:"lastOpened,omitempty"`
	// Local port of the auth-proxy forwarder, reused so a Chrome window that
	// is still open keeps working after the manager restarts.
	FwdPort int `json:"fwdPort,omitempty"`
}

// synced returns the copy of p that is uploaded (local-only fields cleared).
func (p Profile) synced() Profile {
	p.LastOpened, p.FwdPort = nil, 0
	return p
}

type Settings struct {
	ChromePath string `json:"chromePath"`
	// Let the same profile be open on several computers at once (sync only).
	AllowMultiOpen bool `json:"allowMultiOpen"`
	// Chrome UI/website language: "" or "vi" = Vietnamese, "en" = English,
	// "system" = leave Chrome's default.
	ChromeLang string `json:"chromeLang"`
	// Max disk cache per profile in MB: 0 = 200 (default), -1 = no limit.
	CacheMB int `json:"cacheMB"`
}

type dbFile struct {
	Version  int                  `json:"version"`
	Settings Settings             `json:"settings"`
	Profiles []Profile            `json:"profiles"`
	Deleted  map[string]time.Time `json:"deleted,omitempty"` // tombstones for sync
}

// Store keeps all profiles in a single JSON file and saves it atomically.
type Store struct {
	mu       sync.Mutex
	path     string
	dataDir  string
	db       dbFile
	onChange func() // called after a change that must be synced
}

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "profiles"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "profiles.json"), dataDir: dataDir}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.db = dbFile{Version: 1}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.db); err != nil {
			// Keep the broken file instead of overwriting the user's data.
			_ = os.WriteFile(s.path+".broken-"+time.Now().Format("20060102-150405"), b, 0o600)
			return nil, errors.New("profiles.json bị hỏng, đã sao lưu bản lỗi: " + err.Error())
		}
	}
	if s.db.Deleted == nil {
		s.db.Deleted = map[string]time.Time{}
	}
	for i := range s.db.Profiles {
		if s.db.Profiles[i].UpdatedAt.IsZero() {
			s.db.Profiles[i].UpdatedAt = s.db.Profiles[i].CreatedAt
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) changed() {
	if s.onChange != nil {
		go s.onChange()
	}
}

func (s *Store) ProfileDir(id string) string {
	return filepath.Join(s.dataDir, "profiles", id)
}

func (s *Store) Profiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Profile(nil), s.db.Profiles...)
}

func (s *Store) Get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.db.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

func (s *Store) Groups() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, p := range s.db.Profiles {
		g := strings.TrimSpace(p.Group)
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) Add(ps []Profile) error {
	s.mu.Lock()
	now := time.Now()
	for i := range ps {
		ps[i].UpdatedAt = now
	}
	s.db.Profiles = append(s.db.Profiles, ps...)
	err := s.saveLocked()
	s.mu.Unlock()
	s.changed()
	return err
}

// Update changes synced fields of a profile.
func (s *Store) Update(id string, fn func(*Profile)) (Profile, error) {
	p, err := s.update(id, fn, true)
	if err == nil {
		s.changed()
	}
	return p, err
}

// UpdateLocal changes local-only fields (LastOpened, FwdPort).
func (s *Store) UpdateLocal(id string, fn func(*Profile)) (Profile, error) {
	return s.update(id, fn, false)
}

func (s *Store) update(id string, fn func(*Profile), bump bool) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Profiles {
		if s.db.Profiles[i].ID == id {
			fn(&s.db.Profiles[i])
			if bump {
				s.db.Profiles[i].UpdatedAt = time.Now()
			}
			return s.db.Profiles[i], s.saveLocked()
		}
	}
	return Profile{}, errors.New("không tìm thấy hồ sơ")
}

func (s *Store) Delete(ids []string) error {
	s.mu.Lock()
	del := map[string]bool{}
	now := time.Now()
	for _, id := range ids {
		del[id] = true
		s.db.Deleted[id] = now
	}
	kept := s.db.Profiles[:0]
	for _, p := range s.db.Profiles {
		if !del[p.ID] {
			kept = append(kept, p)
		}
	}
	s.db.Profiles = kept
	err := s.saveLocked()
	s.mu.Unlock()
	s.changed()
	return err
}

func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Settings
}

func (s *Store) SetSettings(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Settings = st
	return s.saveLocked()
}

// ---------- sync support ----------

// SyncDoc is what gets uploaded as the "profiles" blob.
type SyncDoc struct {
	Profiles []Profile            `json:"profiles"`
	Deleted  map[string]time.Time `json:"deleted"`
}

func (s *Store) SyncDoc() SyncDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := SyncDoc{Deleted: map[string]time.Time{}}
	for _, p := range s.db.Profiles {
		d.Profiles = append(d.Profiles, p.synced())
	}
	sort.Slice(d.Profiles, func(i, j int) bool { return d.Profiles[i].ID < d.Profiles[j].ID })
	cut := time.Now().Add(-180 * 24 * time.Hour) // forget very old tombstones
	for id, t := range s.db.Deleted {
		if t.After(cut) {
			d.Deleted[id] = t
		}
	}
	return d
}

// Merge applies a remote document: newer UpdatedAt wins per profile, and a
// deletion wins over edits made before it. Returns IDs removed locally.
func (s *Store) Merge(r SyncDoc) (removed []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range r.Deleted {
		if cur, ok := s.db.Deleted[id]; !ok || t.After(cur) {
			s.db.Deleted[id] = t
		}
	}
	idx := map[string]int{}
	for i, p := range s.db.Profiles {
		idx[p.ID] = i
	}
	for _, rp := range r.Profiles {
		if t, ok := s.db.Deleted[rp.ID]; ok && !rp.UpdatedAt.After(t) {
			continue
		}
		if i, ok := idx[rp.ID]; ok {
			lp := s.db.Profiles[i]
			if rp.UpdatedAt.After(lp.UpdatedAt) {
				rp.LastOpened, rp.FwdPort = lp.LastOpened, lp.FwdPort
				s.db.Profiles[i] = rp
			}
			continue
		}
		rp.LastOpened, rp.FwdPort = nil, 0
		s.db.Profiles = append(s.db.Profiles, rp)
		idx[rp.ID] = len(s.db.Profiles) - 1
	}
	kept := s.db.Profiles[:0]
	for _, p := range s.db.Profiles {
		if t, ok := s.db.Deleted[p.ID]; ok && !p.UpdatedAt.After(t) {
			removed = append(removed, p.ID)
			continue
		}
		kept = append(kept, p)
	}
	s.db.Profiles = kept
	return removed, s.saveLocked()
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
