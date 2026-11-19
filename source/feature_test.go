package main

import (
	"testing"
	"time"
)

func TestTOTPRFC6238(t *testing.T) {
	// RFC 6238 test secret "12345678901234567890" in base32, SHA-1.
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		got, left, err := totpCode(secret, time.Unix(ts, 0))
		if err != nil || got != want {
			t.Errorf("t=%d got %s want %s (%v)", ts, got, want, err)
		}
		if left < 1 || left > 30 {
			t.Errorf("remaining %d", left)
		}
	}
	// spaces / lowercase as shown by Facebook
	if got, _, _ := totpCode("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", time.Unix(59, 0)); got != "287082" {
		t.Errorf("formatted secret: %s", got)
	}
	if _, _, err := totpCode("not base32 !!", time.Now()); err == nil {
		t.Error("expected error")
	}
}

func TestTrashAndMetaSync(t *testing.T) {
	a, _ := OpenStore(t.TempDir())
	b, _ := OpenStore(t.TempDir())
	_ = a.Add([]Profile{{ID: "p1", Name: "P1", Account: &Account{UID: "100", TwoFA: "ABC"}}})
	_, _ = b.Merge(a.SyncDoc())
	if p, _ := b.Get("p1"); p.Account == nil || p.Account.UID != "100" {
		t.Fatal("account not synced")
	}
	time.Sleep(5 * time.Millisecond)
	_ = a.SetTrashed([]string{"p1"}, true)
	_ = a.SetMeta(SharedMeta{VPSExpiry: "2027-01-09"})
	_, _ = b.Merge(a.SyncDoc())
	if p, _ := b.Get("p1"); p.TrashedAt == nil {
		t.Fatal("trash not synced")
	}
	if b.Meta().VPSExpiry != "2027-01-09" {
		t.Fatalf("meta not synced: %+v", b.Meta())
	}
	time.Sleep(5 * time.Millisecond)
	_ = b.SetTrashed([]string{"p1"}, false)
	_, _ = a.Merge(b.SyncDoc())
	if p, _ := a.Get("p1"); p.TrashedAt != nil {
		t.Fatal("restore not synced back")
	}
	if fresh, _ := OpenStore(t.TempDir()); fresh.Meta().VPSExpiry != defaultVPSExpiry {
		t.Fatal("default VPS expiry")
	}
}
