package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSiteDataPackUnpack(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	d := filepath.Join(src, "Default")
	write(t, filepath.Join(d, "LocalStorage"), "SQLITE-LS")
	write(t, filepath.Join(d, "Local Storage", "leveldb", "000003.log"), "LEVELDB-LS")
	write(t, filepath.Join(d, "IndexedDB", "https_chat.zalo.me_0.indexeddb.leveldb", "x"), "ZALO")
	write(t, filepath.Join(d, "IndexedDB", "https_www.facebook.com_0.indexeddb.leveldb", "x"), "FB")
	write(t, filepath.Join(d, "Network", "Cookies"), "C")
	// stale data on the target that must be replaced
	write(t, filepath.Join(dst, "Default", "LocalStorage-wal"), "OLD")
	write(t, filepath.Join(dst, "Default", "IndexedDB", "https_chat.zalo.me_0.indexeddb.leveldb", "old"), "OLD")
	write(t, filepath.Join(dst, "Default", "IndexedDB", "https_other.com_0.indexeddb.leveldb", "x"), "KEEP")

	pack, err := PackSiteData(src, siteDomains("zalo.me"))
	if err != nil {
		t.Fatal(err)
	}
	if err := UnpackSiteData(dst, pack); err != nil {
		t.Fatal(err)
	}
	dd := filepath.Join(dst, "Default")
	for p, want := range map[string]string{"LocalStorage": "SQLITE-LS", "Local Storage/leveldb/000003.log": "LEVELDB-LS",
		"IndexedDB/https_chat.zalo.me_0.indexeddb.leveldb/x": "ZALO", "IndexedDB/https_other.com_0.indexeddb.leveldb/x": "KEEP"} {
		b, err := os.ReadFile(filepath.Join(dd, p))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q %v", p, b, err)
		}
	}
	for _, gone := range []string{"LocalStorage-wal", "IndexedDB/https_chat.zalo.me_0.indexeddb.leveldb/old",
		"IndexedDB/https_www.facebook.com_0.indexeddb.leveldb", "Network/Cookies"} {
		if exists(filepath.Join(dd, gone)) {
			t.Errorf("%s should not exist", gone)
		}
	}
	if got := siteDomains("https://chat.zalo.me/, web.telegram.org"); len(got) != 2 || got[0] != "chat.zalo.me" {
		t.Errorf("domains %v", got)
	}
}
