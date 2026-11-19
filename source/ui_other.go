//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
)

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// On Linux/macOS there is no native window: open the printed URL in a browser.
func runUI(url, dataDir string) {
	fmt.Println("Data:", dataDir)
	fmt.Println("Open:", url)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	quitUI = func() { c <- os.Interrupt }
	<-c
}
