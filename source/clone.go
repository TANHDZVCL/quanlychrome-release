package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------- extension IDs ----------

var reExtID = regexp.MustCompile(`[a-p]{32}`)

// ParseExtensionIDs pulls Chrome Web Store IDs out of pasted links or IDs.
func ParseExtensionIDs(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range reExtID.FindAllString(strings.ToLower(text), -1) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func validExtIDs(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) == 32 && reExtID.MatchString(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// InstalledExtensions lists the Web Store extensions installed in a
// profile, read from Chrome's (Secure) Preferences.
func InstalledExtensions(userDataDir string) []string {
	var ids []string
	for _, name := range []string{"Secure Preferences", "Preferences"} {
		b, err := os.ReadFile(filepath.Join(userDataDir, "Default", name))
		if err != nil {
			continue
		}
		var prefs struct {
			Extensions struct {
				Settings map[string]struct {
					Location     int  `json:"location"`
					FromWebstore bool `json:"from_webstore"`
					Manifest     struct {
						UpdateURL string `json:"update_url"`
					} `json:"manifest"`
				} `json:"settings"`
			} `json:"extensions"`
		}
		if json.Unmarshal(b, &prefs) != nil {
			continue
		}
		for id, s := range prefs.Extensions.Settings {
			// location 1 = installed by the user (Web Store)
			if s.Location == 1 && (s.FromWebstore || strings.Contains(s.Manifest.UpdateURL, "google.com")) {
				ids = append(ids, id)
			}
		}
	}
	return validExtIDs(ids)
}

func unionIDs(a, b []string) []string { return validExtIDs(append(append([]string{}, a...), b...)) }

func webStoreURL(id string) string { return "https://chromewebstore.google.com/detail/" + id }

// ---------- profile cloning ----------

// Things that must NOT be copied: logins, cookies, history, site storage,
// caches. Extensions, their data and Chrome settings are kept.
var cloneSkip = map[string]bool{
	"cookies": true, "cookies-journal": true, "extension cookies": true, "extension cookies-journal": true,
	"network": true, "login data": true, "login data-journal": true, "login data for account": true,
	"login data for account-journal": true, "web data": true, "web data-journal": true, "account web data": true,
	"account web data-journal": true, "history": true, "history-journal": true, "visited links": true,
	"top sites": true, "top sites-journal": true, "shortcuts": true, "shortcuts-journal": true,
	"network action predictor": true, "network action predictor-journal": true, "sessions": true,
	"session storage": true, "local storage": true, "webstorage": true, "file system": true, "blob_storage": true,
	"storage": true, "cache": true, "code cache": true, "gpucache": true, "dawncache": true,
	"dawngraphitecache": true, "dawnwebgpucache": true, "trust tokens": true, "trust tokens-journal": true,
	"safe browsing network": true, "download service": true, "media history": true, "media history-journal": true,
	"bookmarks": true, "bookmarks.bak": true, "favicons": true, "favicons-journal": true,
	"lock": true, "chrome_debug.log": true, "shared_proto_db": true, "commerce_subscription_db": true,
	"dips": true, "dips-journal": true, "affiliation database": true, "affiliation database-journal": true,
	"autofillstrikedatabase": true, "autofillaimodelcache": true, "budgetdatabase": true, "gcm store": true,
	"sync data": true, "platform_notifications": true, "segmentation_platform": true, "collaboration": true,
	"datasharing": true, "clientcertificates": true, "reporting and nel": true, "reporting and nel-journal": true,
	// newer Chrome keeps web storage in SQLite files
	"localstorage": true, "localstorage-journal": true, "localstorage-wal": true, "localstorage-shm": true,
	"sessionstorage": true, "sessionstorage-journal": true, "sessionstorage-wal": true, "sessionstorage-shm": true,
}

func skipInDefault(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	first := strings.ToLower(parts[0])
	if cloneSkip[first] {
		return true
	}
	switch first {
	case "indexeddb": // keep only the extensions' own databases
		return len(parts) > 1 && !strings.HasPrefix(parts[1], "chrome-extension_")
	case "service worker":
		return len(parts) > 1 && strings.EqualFold(parts[1], "CacheStorage")
	}
	return false
}

// CloneProfileDir copies src (a closed profile's user-data-dir) to dst,
// keeping extensions and settings but not logins/cookies/history.
func CloneProfileDir(src, dst string) error {
	srcDef := filepath.Join(src, "Default")
	if _, err := os.Stat(srcDef); err != nil {
		return errors.New("hồ sơ gốc chưa từng được mở trên máy này — hãy mở nó 1 lần (để cài tiện ích) rồi nhân bản")
	}
	dstDef := filepath.Join(dst, "Default")
	if err := os.MkdirAll(dstDef, 0o755); err != nil {
		return err
	}
	err := filepath.WalkDir(srcDef, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		rel, _ := filepath.Rel(srcDef, p)
		if rel == "." {
			return nil
		}
		if skipInDefault(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dstDef, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
	if err != nil {
		return err
	}
	_ = scrubPreferences(filepath.Join(dstDef, "Preferences"))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return nil // locked/vanished file: skip it
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// scrubPreferences removes the Google account sign-in of the source profile.
func scrubPreferences(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for _, k := range []string{"account_info", "account_tracker_service_last_update", "signin", "sync", "gaia_cookie", "google"} {
		delete(m, k)
	}
	if prof, ok := m["profile"].(map[string]any); ok {
		delete(prof, "exit_type")
		delete(prof, "exited_cleanly")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
