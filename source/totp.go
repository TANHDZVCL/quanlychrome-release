package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// totpCode returns the current 6-digit 2FA code (RFC 6238, 30 s, SHA-1) for
// a base32 secret such as the ones Facebook/Google show, and how many seconds
// it stays valid.
func totpCode(secret string, now time.Time) (string, int, error) {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(secret)))
	s = strings.TrimRight(s, "=")
	if s == "" {
		return "", 0, errors.New("chưa nhập mã 2FA")
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil || len(key) == 0 {
		return "", 0, errors.New("mã 2FA không hợp lệ (cần chuỗi bí mật dạng chữ A–Z và số 2–7)")
	}
	counter := uint64(now.Unix() / 30)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", v%1000000), 30 - int(now.Unix()%30), nil
}
