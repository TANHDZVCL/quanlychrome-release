//go:build !windows

package main

import "encoding/base64"

// Non-Windows builds are for development only: no OS key protection.
func protectKey(b []byte) (string, error)   { return base64.StdEncoding.EncodeToString(b), nil }
func unprotectKey(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
