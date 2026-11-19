package main

import (
	"path/filepath"
	"testing"
)

func TestClearCacheKeepsLogin(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "Default")
	write(t, filepath.Join(d, "Cache", "Cache_Data", "f_000001"), "0123456789")
	write(t, filepath.Join(d, "Code Cache", "js", "x"), "abcde")
	write(t, filepath.Join(d, "Service Worker", "CacheStorage", "x"), "abc")
	write(t, filepath.Join(d, "Service Worker", "Database", "x"), "sw-db")
	write(t, filepath.Join(d, "Network", "Cookies"), "COOKIES")
	write(t, filepath.Join(d, "Local Storage", "leveldb", "x"), "LS")
	write(t, filepath.Join(d, "IndexedDB", "x"), "IDB")
	write(t, filepath.Join(d, "Login Data"), "PW")
	write(t, filepath.Join(d, "Extensions", "a", "m.json"), "{}")
	write(t, filepath.Join(dir, "Local State"), "{}")
	if got := dirSize(dir); got == 0 {
		t.Fatal("size 0")
	}
	freed := ClearCache(dir)
	if freed != 18 {
		t.Errorf("freed = %d, want 18", freed)
	}
	for _, gone := range []string{"Cache", "Code Cache", "Service Worker/CacheStorage"} {
		if exists(filepath.Join(d, gone)) {
			t.Errorf("%s not removed", gone)
		}
	}
	for _, kept := range []string{"Network/Cookies", "Local Storage/leveldb/x", "IndexedDB/x", "Login Data",
		"Extensions/a/m.json", "Service Worker/Database/x"} {
		if !exists(filepath.Join(d, kept)) {
			t.Errorf("%s must be kept", kept)
		}
	}
	if !exists(filepath.Join(dir, "Local State")) {
		t.Error("Local State must be kept")
	}
}

func TestCacheLimit(t *testing.T) {
	if cacheLimitMB(0) != 200 || cacheLimitMB(-1) != 0 || cacheLimitMB(500) != 500 {
		t.Fatal("cacheLimitMB")
	}
}
