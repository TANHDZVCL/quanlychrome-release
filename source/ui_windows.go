//go:build windows

package main

import (
	"os"
	"path/filepath"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

func fatal(msg string) {
	t, _ := windows.UTF16PtrFromString(appName)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}

func runUI(url, dataDir string) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     true, // keeps the right-click menu (copy/paste) available
		AutoFocus: true,
		DataPath:  filepath.Join(dataDir, "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  appName + " v" + appVersion,
			Width:  1280,
			Height: 800,
			IconId: 1,
			Center: true,
		},
	})
	if w == nil {
		fatal("Máy chưa có Microsoft Edge WebView2 Runtime.\n\nHãy tải và cài tại:\nhttps://developer.microsoft.com/microsoft-edge/webview2/\n(chọn \"Evergreen Bootstrapper\"), rồi mở lại phần mềm.")
	}
	defer w.Destroy()
	quitUI = func() { w.Dispatch(w.Terminate) }
	w.Navigate(url)
	w.Run()
}
