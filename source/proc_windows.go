//go:build windows

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func chromeCandidates() []string {
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData", "ProgramW6432"} {
		if base := os.Getenv(env); base != "" {
			out = append(out, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	return out
}

// Chrome holds <user-data-dir>\lockfile open (write access, no write sharing)
// for as long as the profile is running.
func profileLocked(dir string) bool {
	lf := filepath.Join(dir, "lockfile")
	if _, err := os.Stat(lf); err != nil {
		return false
	}
	f, err := os.OpenFile(lf, os.O_WRONLY, 0)
	if err != nil {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	f.Close()
	return false
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// closeProcessTree asks Chrome to close its windows (so tabs/cookies are
// saved), then force-kills it if it is still alive a few seconds later.
func closeProcessTree(pid int) {
	p := strconv.Itoa(pid)
	_ = hidden(exec.Command("taskkill", "/PID", p, "/T")).Run()
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(500 * time.Millisecond)
			if !processAlive(pid) {
				return
			}
		}
		log.Printf("Chrome pid %s không tự đóng sau 20 giây: buộc tắt (/F)", p)
		_ = hidden(exec.Command("taskkill", "/PID", p, "/T", "/F")).Run()
	}()
}

func closeByUserDataDir(dir string) error {
	needle := strings.ReplaceAll("--user-data-dir="+dir, "'", "''")
	script := "$d='" + needle + "'; Get-CimInstance Win32_Process -Filter \"Name='chrome.exe'\" | " +
		"Where-Object { $_.CommandLine -and $_.CommandLine.Contains($d) -and -not $_.CommandLine.Contains('--type=') } | " +
		"ForEach-Object { $_.ProcessId }"
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Output()
	if err != nil {
		return errors.New("không tìm được cửa sổ Chrome của hồ sơ này")
	}
	found := false
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			closeProcessTree(pid)
			found = true
		}
	}
	if !found {
		return errors.New("không tìm được cửa sổ Chrome của hồ sơ này")
	}
	return nil
}

func openFolder(dir string) error {
	return exec.Command("explorer.exe", dir).Start()
}
