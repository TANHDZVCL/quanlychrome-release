package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache folders inside a Chrome user-data-dir: only re-downloadable copies
// of web resources. Cookies, site storage, extensions etc. are NOT here.
var cacheDirs = []string{
	"Default/Cache", "Default/Code Cache", "Default/GPUCache", "Default/DawnCache",
	"Default/DawnGraphiteCache", "Default/DawnWebGPUCache", "Default/Service Worker/CacheStorage",
	"GrShaderCache", "ShaderCache", "GraphiteDawnCache",
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// ClearCache deletes the cache folders of a closed profile and returns the
// number of bytes freed.
func ClearCache(userDataDir string) int64 {
	var freed int64
	for _, rel := range cacheDirs {
		p := filepath.Join(userDataDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err != nil {
			continue
		}
		size := dirSize(p)
		if os.RemoveAll(p) == nil {
			freed += size
		}
	}
	return freed
}

// sizeCache remembers profile folder sizes (walking them is slow).
type sizeCache struct {
	mu    sync.Mutex
	sizes map[string]int64
	at    map[string]time.Time
	busy  bool
}

var profileSizes = &sizeCache{sizes: map[string]int64{}, at: map[string]time.Time{}}

func (c *sizeCache) Get() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.sizes))
	for k, v := range c.sizes {
		out[k] = v
	}
	return out
}

// Refresh recomputes sizes older than maxAge in the background.
func (c *sizeCache) Refresh(s *Store, maxAge time.Duration) {
	c.mu.Lock()
	if c.busy {
		c.mu.Unlock()
		return
	}
	c.busy = true
	c.mu.Unlock()
	go func() {
		defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()
		for _, p := range s.Profiles() {
			c.mu.Lock()
			fresh := time.Since(c.at[p.ID]) < maxAge
			c.mu.Unlock()
			if fresh {
				continue
			}
			n := dirSize(s.ProfileDir(p.ID))
			c.mu.Lock()
			c.sizes[p.ID], c.at[p.ID] = n, time.Now()
			c.mu.Unlock()
		}
	}()
}

func (c *sizeCache) Forget(id string) {
	c.mu.Lock()
	delete(c.at, id)
	c.mu.Unlock()
}
