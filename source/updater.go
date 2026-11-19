package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Self-update: the VPS serves /tai/version.json and /tai/ChromeManager.exe.
// When a newer version is published there, the app shows an "Update"
// button; pressing it downloads the new exe, swaps it in and restarts.

const defaultServer = "https://49-236-211-75.sslip.io"

type versionInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Notes   string `json:"notes"`
	File    string `json:"file"` // default ChromeManager.exe
}

var (
	quitUI      = func() {} // set by the UI: closes the window
	restartMu   sync.Mutex
	restartPath string // exe to start after the window closes
)

// newerVersion reports whether a > b for versions like "2.10" / "2.4.1".
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func (s *Server) updateBase() string {
	if srv := s.sync.Status().Server; srv != "" {
		return srv
	}
	return defaultServer
}

func fetchVersion(base string) (*versionInfo, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(base + "/tai/version.json")
	if err != nil {
		return nil, fmt.Errorf("không kiểm tra được bản mới: %s", friendlyNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil // nothing published yet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("máy chủ trả lỗi %d khi kiểm tra bản mới", resp.StatusCode)
	}
	var v versionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return nil, errors.New("version.json không hợp lệ")
	}
	if v.File == "" {
		v.File = "ChromeManager.exe"
	}
	return &v, nil
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	res := map[string]any{"current": appVersion}
	v, err := fetchVersion(s.updateBase())
	if err != nil {
		res["error"] = err.Error()
	} else if v != nil {
		res["latest"], res["notes"] = v.Version, v.Notes
		res["available"] = newerVersion(v.Version, appVersion)
	}
	writeJSON(w, res)
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	base := s.updateBase()
	v, err := fetchVersion(base)
	if err != nil || v == nil {
		if err == nil {
			err = errors.New("chưa có bản cập nhật trên máy chủ")
		}
		writeErr(w, 400, err.Error())
		return
	}
	if !newerVersion(v.Version, appVersion) {
		writeErr(w, 400, "Đang dùng bản mới nhất ("+appVersion+")")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if strings.ContainsAny(v.File, `/\`) {
		writeErr(w, 400, "tên file cập nhật không hợp lệ")
		return
	}
	// Download next to the exe, check the hash, then swap.
	c := &http.Client{Timeout: 10 * time.Minute}
	resp, err := c.Get(base + "/tai/" + v.File)
	if err != nil {
		writeErr(w, 502, "Không tải được bản mới: "+friendlyNetErr(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("Không tải được bản mới (lỗi %d)", resp.StatusCode))
		return
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		writeErr(w, 500, "Không ghi được file (thư mục chỉ đọc?): "+err.Error())
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 200<<20))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		writeErr(w, 502, "Tải bản mới bị gián đoạn: "+err.Error())
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, v.SHA256) {
		os.Remove(tmp)
		writeErr(w, 502, "File tải về bị lỗi (mã kiểm tra không khớp), hãy thử lại")
		return
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil { // Windows allows renaming a running exe
		os.Remove(tmp)
		writeErr(w, 500, "Không thay được file: "+err.Error())
		return
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(old, exe)
		writeErr(w, 500, "Không thay được file: "+err.Error())
		return
	}
	log.Printf("đã tải bản %s, khởi động lại", v.Version)
	restartMu.Lock()
	restartPath = exe
	restartMu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "version": v.Version})
	go func() {
		time.Sleep(800 * time.Millisecond) // let the UI show "restarting"
		quitUI()
	}()
}

// restartIfUpdated starts the new exe after the old window has closed.
func restartIfUpdated() {
	restartMu.Lock()
	p := restartPath
	restartMu.Unlock()
	if p == "" {
		return
	}
	cmd := exec.Command(p, "--after-update")
	if err := cmd.Start(); err != nil {
		log.Printf("không khởi động lại được: %v", err)
	}
}

// cleanupAfterUpdate runs at start: wait for the old process to exit, then
// remove the previous exe.
func cleanupAfterUpdate() {
	for _, a := range os.Args[1:] {
		if a == "--after-update" {
			time.Sleep(2500 * time.Millisecond)
		}
	}
	if exe, err := os.Executable(); err == nil {
		go func() {
			time.Sleep(5 * time.Second)
			_ = os.Remove(exe + ".old")
		}()
	}
}
