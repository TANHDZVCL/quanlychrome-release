package main

import (
	"log"
	"os"
	"sync"
	"time"
)

const (
	healthEvery = 3 * time.Hour
	trashDays   = 7
)

type proxyHealth struct {
	ProxyCheck
	At time.Time `json:"at"`
	// key of the proxy that was checked: a result is shown only while the
	// profile still uses that proxy.
	Key string `json:"key"`
}

func proxyKey(p Proxy) string { return p.Type + "://" + p.Host + ":" + itoa(p.Port) + "@" + p.User }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type healthStore struct {
	mu sync.Mutex
	m  map[string]proxyHealth
}

var health = &healthStore{m: map[string]proxyHealth{}}

func (h *healthStore) Set(id string, p Proxy, c ProxyCheck) {
	h.mu.Lock()
	h.m[id] = proxyHealth{ProxyCheck: c, At: time.Now(), Key: proxyKey(p)}
	h.mu.Unlock()
}

// For returns the results that still match each profile's current proxy.
func (h *healthStore) For(ps []Profile) map[string]proxyHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]proxyHealth{}
	for _, p := range ps {
		if r, ok := h.m[p.ID]; ok && p.Proxy.Enabled() && r.Key == proxyKey(p.Proxy) {
			out[p.ID] = r
		}
	}
	return out
}

// CheckAll tests the proxy of every active profile (4 at a time).
func (m *Manager) CheckAll() {
	var todo []Profile
	for _, p := range m.store.Profiles() {
		if p.TrashedAt == nil && p.Proxy.Enabled() {
			todo = append(todo, p)
		}
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	bad := 0
	var mu sync.Mutex
	for _, p := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(p Profile) {
			defer wg.Done()
			defer func() { <-sem }()
			c := CheckProxy(p.Proxy)
			health.Set(p.ID, p.Proxy, c)
			if !c.OK {
				mu.Lock()
				bad++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if len(todo) > 0 {
		log.Printf("kiểm tra proxy định kỳ: %d hồ sơ, %d lỗi", len(todo), bad)
	}
}

// maintenance runs the periodic proxy checks and empties the trash.
func (m *Manager) maintenance() {
	time.Sleep(90 * time.Second)
	lastCheck := time.Time{}
	for {
		if time.Since(lastCheck) >= healthEvery {
			lastCheck = time.Now()
			m.CheckAll()
		}
		m.purgeTrash()
		time.Sleep(10 * time.Minute)
	}
}

func (m *Manager) purgeTrash() {
	var ids []string
	for _, p := range m.store.Profiles() {
		if p.TrashedAt != nil && time.Since(*p.TrashedAt) > trashDays*24*time.Hour && !m.IsRunning(p.ID) {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := m.store.Delete(ids); err != nil {
		return
	}
	for _, id := range ids {
		_ = os.RemoveAll(m.store.ProfileDir(id))
	}
	log.Printf("đã xóa vĩnh viễn %d hồ sơ trong thùng rác quá %d ngày", len(ids), trashDays)
}
