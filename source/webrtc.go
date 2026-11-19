package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// setWebRTCPolicy writes Chrome's WebRTC IP-handling preference into the
// profile before Chrome starts. Newer Chrome ignores the command-line switch,
// so without this WebRTC can reveal the real IP even behind a proxy.
//
// With a proxy: "disable_non_proxied_udp" (WebRTC may only go through the
// proxy). Without one: back to Chrome's default so calls keep working.
func setWebRTCPolicy(userDataDir string, proxied bool) error {
	def := filepath.Join(userDataDir, "Default")
	if err := os.MkdirAll(def, 0o755); err != nil {
		return err
	}
	path := filepath.Join(def, "Preferences")
	prefs := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &prefs); err != nil {
			return err // never overwrite a file we cannot parse
		}
	}
	webrtc, _ := prefs["webrtc"].(map[string]any)
	if webrtc == nil {
		webrtc = map[string]any{}
	}
	if proxied {
		webrtc["ip_handling_policy"] = "disable_non_proxied_udp"
		webrtc["multiple_routes_enabled"] = false
		webrtc["nonproxied_udp_enabled"] = false
	} else {
		if webrtc["ip_handling_policy"] == nil {
			return nil // never set by us: leave the file untouched
		}
		webrtc["ip_handling_policy"] = "default"
		webrtc["multiple_routes_enabled"] = true
		webrtc["nonproxied_udp_enabled"] = true
	}
	prefs["webrtc"] = webrtc
	out, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
