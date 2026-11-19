package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Site data sync: besides cookies, some web apps (Zalo Web, Telegram Web…)
// keep their login in Local Storage / IndexedDB. For profiles that opt in,
// those folders are packed when Chrome is closed and restored before it
// starts on another computer.

const maxSitePack = 50 << 20

func siteDomains(s string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' }) {
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		d = strings.Trim(strings.SplitN(d, "/", 2)[0], ".")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// sitePaths lists the folders (relative to the user-data-dir) to sync.
func sitePaths(dir string, domains []string) []string {
	// Older Chrome: "Local Storage" (LevelDB folder); newer: "LocalStorage" (SQLite file).
	paths := []string{"Default/Local Storage", "Default/LocalStorage", "Default/LocalStorage-journal", "Default/LocalStorage-wal"}
	entries, _ := os.ReadDir(filepath.Join(dir, "Default", "IndexedDB"))
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		for _, d := range domains {
			if strings.Contains(name, d) {
				paths = append(paths, "Default/IndexedDB/"+e.Name())
				break
			}
		}
	}
	return paths
}

// PackSiteData returns a tar.gz of the site storage of a CLOSED profile.
func PackSiteData(dir string, domains []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, rel := range sitePaths(dir, domains) {
		root := filepath.Join(dir, filepath.FromSlash(rel))
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || d.Name() == "LOCK" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			r, _ := filepath.Rel(dir, p)
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(r), Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			return err
		})
		if err != nil {
			return nil, err
		}
		if buf.Len() > maxSitePack {
			return nil, fmt.Errorf("dữ liệu web quá lớn (> %d MB)", maxSitePack>>20)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > maxSitePack {
		return nil, fmt.Errorf("dữ liệu web quá lớn (> %d MB)", maxSitePack>>20)
	}
	return buf.Bytes(), nil
}

// UnpackSiteData replaces the site storage of a CLOSED profile.
func UnpackSiteData(dir string, data []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	cleared := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if strings.Contains(name, "..") || !(strings.HasPrefix(name, "Default/Local Storage/") ||
			strings.HasPrefix(name, "Default/IndexedDB/") || strings.HasPrefix(name, "Default/LocalStorage")) {
			continue // only the folders we pack
		}
		if strings.HasPrefix(name, "Default/LocalStorage") { // SQLite file + its journal/wal
			if !cleared["sqlite-ls"] {
				for _, f := range []string{"LocalStorage", "LocalStorage-journal", "LocalStorage-wal", "LocalStorage-shm"} {
					_ = os.Remove(filepath.Join(dir, "Default", f))
				}
				cleared["sqlite-ls"] = true
			}
			if err := writeTarFile(filepath.Join(dir, filepath.FromSlash(name)), tr, h.Size); err != nil {
				return err
			}
			continue
		}
		// Replace each top folder as a whole (old files must not linger).
		parts := strings.SplitN(name, "/", 4)
		top := strings.Join(parts[:3], "/")
		if parts[1] == "Local Storage" {
			top = "Default/Local Storage"
		}
		if !cleared[top] {
			_ = os.RemoveAll(filepath.Join(dir, filepath.FromSlash(top)))
			cleared[top] = true
		}
		if err := writeTarFile(filepath.Join(dir, filepath.FromSlash(name)), tr, h.Size); err != nil {
			return err
		}
	}
}

func writeTarFile(target string, r io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
