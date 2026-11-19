package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

const appName = "Quản lý Chrome"

func dataDir() string {
	if d := os.Getenv("CM_DATA"); d != "" {
		return d
	}
	// Keep data next to the .exe so the whole folder can be copied/backed up.
	if exe, err := os.Executable(); err == nil {
		d := filepath.Join(filepath.Dir(exe), "ChromeManagerData")
		if os.MkdirAll(d, 0o755) == nil {
			probe := filepath.Join(d, ".write-test")
			if os.WriteFile(probe, []byte("ok"), 0o600) == nil {
				os.Remove(probe)
				return d
			}
		}
	}
	base, _ := os.UserConfigDir()
	return filepath.Join(base, "ChromeManager")
}

// setupLog writes the log to <data>/app.log so problems can be diagnosed
// (the Windows build has no console).
func setupLog(dir string) {
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "app.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
	log.Printf("===== khởi động %s =====", appVersion)
}

const appVersion = "2.9"

func main() {
	cleanupAfterUpdate()
	dir := dataDir()
	setupLog(dir)
	store, err := OpenStore(dir)
	if err != nil {
		fatal("Không mở được dữ liệu: " + err.Error())
	}
	syncer := NewSyncer(store, dir)
	mgr := NewManager(store, syncer)

	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	srv := &Server{store: store, mgr: mgr, sync: syncer, token: hex.EncodeToString(tok)}

	addr := "127.0.0.1:0"
	if a := os.Getenv("CM_ADDR"); a != "" {
		addr = a
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("Không khởi động được: " + err.Error())
	}
	go func() { _ = http.Serve(ln, srv.Handler()) }()

	url := fmt.Sprintf("http://%s/#%s", ln.Addr().String(), srv.token)
	runUI(url, dir)
	mgr.Shutdown()
	restartIfUpdated()
}
