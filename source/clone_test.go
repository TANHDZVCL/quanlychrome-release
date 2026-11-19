package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestCloneProfileDir(t *testing.T) {
	ext := "naciaagbkifhpnoodlkhbejjldaiffcm"
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "clone")
	d := filepath.Join(src, "Default")
	write(t, filepath.Join(d, "Extensions", ext, "1.0_0", "manifest.json"), "{}")
	write(t, filepath.Join(d, "Local Extension Settings", ext, "000003.log"), "ext-data")
	write(t, filepath.Join(d, "IndexedDB", "chrome-extension_"+ext+"_0.indexeddb.leveldb", "x"), "ext-idb")
	write(t, filepath.Join(d, "IndexedDB", "https_www.facebook.com_0.indexeddb.leveldb", "x"), "site-idb")
	write(t, filepath.Join(d, "Network", "Cookies"), "COOKIES")
	write(t, filepath.Join(d, "Cookies"), "COOKIES")
	write(t, filepath.Join(d, "Login Data"), "PASSWORDS")
	write(t, filepath.Join(d, "History"), "HISTORY")
	write(t, filepath.Join(d, "Local Storage", "leveldb", "x"), "site-ls")
	write(t, filepath.Join(d, "Service Worker", "Database", "x"), "sw-db")
	write(t, filepath.Join(d, "Service Worker", "CacheStorage", "x"), "sw-cache")
	write(t, filepath.Join(d, "Cache", "Cache_Data", "x"), "cache")
	write(t, filepath.Join(d, "Bookmarks"), "{}")
	write(t, filepath.Join(d, "Secure Preferences"), `{"extensions":{"settings":{"`+ext+`":{"location":1,"from_webstore":true},"mhjfbmdgcfjbbpaeojofohoefgiehjai":{"location":5}}}}`)
	write(t, filepath.Join(d, "Preferences"), `{"account_info":[{"email":"a@b.c"}],"signin":{"x":1},"profile":{"name":"P","exit_type":"Crashed"},"extensions":{"theme":{}}}`)
	write(t, filepath.Join(src, "Local State"), `{"os_crypt":{"encrypted_key":"SECRET"}}`)

	if got := InstalledExtensions(src); len(got) != 1 || got[0] != ext {
		t.Fatalf("installed = %v", got)
	}
	if err := CloneProfileDir(src, dst); err != nil {
		t.Fatal(err)
	}
	c := filepath.Join(dst, "Default")
	keep := []string{"Extensions/" + ext + "/1.0_0/manifest.json", "Local Extension Settings/" + ext + "/000003.log",
		"IndexedDB/chrome-extension_" + ext + "_0.indexeddb.leveldb/x", "Service Worker/Database/x", "Secure Preferences", "Preferences"}
	drop := []string{"Network", "Cookies", "Login Data", "History", "Local Storage", "Cache", "Bookmarks",
		"IndexedDB/https_www.facebook.com_0.indexeddb.leveldb", "Service Worker/CacheStorage"}
	for _, k := range keep {
		if !exists(filepath.Join(c, k)) {
			t.Errorf("missing %s", k)
		}
	}
	for _, k := range drop {
		if exists(filepath.Join(c, k)) {
			t.Errorf("should not copy %s", k)
		}
	}
	if exists(filepath.Join(dst, "Local State")) {
		t.Error("Local State (cookie key) must not be copied")
	}
	var prefs map[string]any
	b, _ := os.ReadFile(filepath.Join(c, "Preferences"))
	_ = json.Unmarshal(b, &prefs)
	if _, ok := prefs["account_info"]; ok {
		t.Error("account_info kept")
	}
	if prof, _ := prefs["profile"].(map[string]any); prof["name"] != "P" || prof["exit_type"] != nil {
		t.Errorf("profile prefs wrong: %v", prof)
	}
	if got := InstalledExtensions(dst); len(got) != 1 {
		t.Errorf("clone extensions = %v", got)
	}
}

func TestParseExtensionIDs(t *testing.T) {
	ids := ParseExtensionIDs("https://chromewebstore.google.com/detail/get-token-cookie/naciaagbkifhpnoodlkhbejjldaiffcm\nnaciaagbkifhpnoodlkhbejjldaiffcm?hl=vi")
	if len(ids) != 1 || ids[0] != "naciaagbkifhpnoodlkhbejjldaiffcm" {
		t.Fatal(ids)
	}
}
