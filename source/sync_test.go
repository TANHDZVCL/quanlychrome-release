package main

import (
	"testing"
	"time"
)

func TestMergeCookies(t *testing.T) {
	base := cookieList{{"domain": ".a.com", "path": "/", "name": "x", "value": "1"}, {"domain": ".a.com", "path": "/", "name": "y", "value": "1"}}
	top := cookieList{{"domain": ".a.com", "path": "/", "name": "x", "value": "2"}, {"domain": ".b.com", "path": "/", "name": "z", "value": "3"}}
	m := mergeCookies(base, top)
	if len(m) != 3 || m[0]["value"] != "2" || m[1]["value"] != "1" || m[2]["name"] != "z" {
		t.Fatalf("bad merge %v", m)
	}
}

func TestStoreMerge(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = s.Add([]Profile{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}})
	_, _ = s.UpdateLocal("a", func(p *Profile) { p.FwdPort = 5555 })
	now := time.Now().Add(time.Minute)
	removed, err := s.Merge(SyncDoc{
		Profiles: []Profile{{ID: "a", Name: "A2", UpdatedAt: now}, {ID: "c", Name: "C", UpdatedAt: now}, {ID: "b", Name: "OLD", UpdatedAt: old}},
		Deleted:  map[string]time.Time{"b": now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("removed %v", removed)
	}
	a, _ := s.Get("a")
	if a.Name != "A2" || a.FwdPort != 5555 {
		t.Fatalf("a = %+v", a)
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("c missing")
	}
	// a stale remote copy must not override a newer local edit
	_, _ = s.Update("a", func(p *Profile) { p.Name = "local" })
	_, _ = s.Merge(SyncDoc{Profiles: []Profile{{ID: "a", Name: "stale", UpdatedAt: now.Add(-2 * time.Minute)}}})
	if a, _ := s.Get("a"); a.Name != "local" {
		t.Fatalf("stale overwrite: %+v", a)
	}
}

func TestCryptoRoundTrip(t *testing.T) {
	_, k := deriveKeys("User", "pw")
	_, k2 := deriveKeys("user", "pw")
	if string(k) != string(k2) {
		t.Fatal("username must be case-insensitive")
	}
	enc, _ := sealJSON(k, map[string]int{"a": 1})
	var out map[string]int
	if err := unsealJSON(k, enc, &out); err != nil || out["a"] != 1 {
		t.Fatal(err)
	}
	_, bad := deriveKeys("user", "pw2")
	if unsealJSON(bad, enc, &out) == nil {
		t.Fatal("wrong key must fail")
	}
	bk, _ := sealBackup("secret12", map[string]int{"b": 2})
	if openBackup("nope", bk, &out) == nil {
		t.Fatal("wrong backup pw must fail")
	}
	if err := openBackup("secret12", bk, &out); err != nil || out["b"] != 2 {
		t.Fatal(err)
	}
}
