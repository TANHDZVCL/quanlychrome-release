//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func chromeCandidates() []string {
	out := []string{"/opt/pw-browsers/chromium"}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			out = append(out, p)
		}
	}
	out = append(out, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	return out
}

// Chrome on Linux/macOS keeps a SingletonLock symlink "<host>-<pid>".
func lockPID(dir string) int {
	target, err := os.Readlink(filepath.Join(dir, "SingletonLock"))
	if err != nil {
		return 0
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return 0
	}
	pid, _ := strconv.Atoi(target[i+1:])
	return pid
}

func profileLocked(dir string) bool {
	pid := lockPID(dir)
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func hidden(cmd *exec.Cmd) *exec.Cmd { return cmd }

func closeProcessTree(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

func closeByUserDataDir(dir string) error {
	pid := lockPID(dir)
	if pid <= 0 {
		return errors.New("không tìm được Chrome của hồ sơ này")
	}
	closeProcessTree(pid)
	return nil
}

func openFolder(dir string) error { return exec.Command("xdg-open", dir).Start() }

func openURL(u string) error { return exec.Command("xdg-open", u).Start() }
