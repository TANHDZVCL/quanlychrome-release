package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWebRTCPolicy(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Default", "Preferences"), `{"profile":{"name":"X"},"webrtc":{"other":1}}`)
	if err := setWebRTCPolicy(dir, true); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	_ = json.Unmarshal(b, &m)
	w := m["webrtc"].(map[string]any)
	if w["ip_handling_policy"] != "disable_non_proxied_udp" || w["other"] != float64(1) || m["profile"].(map[string]any)["name"] != "X" {
		t.Fatalf("prefs: %s", b)
	}
	_ = setWebRTCPolicy(dir, false)
	b, _ = os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	_ = json.Unmarshal(b, &m)
	if m["webrtc"].(map[string]any)["ip_handling_policy"] != "default" {
		t.Fatalf("not reset: %s", b)
	}
	// new profile: file created
	d2 := t.TempDir()
	if err := setWebRTCPolicy(d2, true); err != nil || !exists(filepath.Join(d2, "Default", "Preferences")) {
		t.Fatal("not created", err)
	}
	// broken file is left alone
	d3 := t.TempDir()
	write(t, filepath.Join(d3, "Default", "Preferences"), "{broken")
	if setWebRTCPolicy(d3, true) == nil {
		t.Fatal("expected error on broken prefs")
	}
}
