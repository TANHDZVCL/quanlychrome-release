package main

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// deriveKeys turns username+password into two independent keys:
//   - authKey is sent to the server to log in (the server stores its hash);
//   - wrapKey never leaves this computer and decrypts the account master key.
//
// The server therefore cannot decrypt anything.
func deriveKeys(username, password string) (authKey, wrapKey []byte) {
	salt := []byte("quanlychrome/v1/" + strings.ToLower(strings.TrimSpace(username)))
	k := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 64)
	return k[:32], k[32:]
}

func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{1}, nonce...)
	return gcm.Seal(out, nonce, plain, nil), nil
}

var errDecrypt = errors.New("không giải mã được (sai mật khẩu hoặc dữ liệu hỏng)")

func unseal(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < 1+gcm.NonceSize()+gcm.Overhead() || data[0] != 1 {
		return nil, errDecrypt
	}
	ns := gcm.NonceSize()
	plain, err := gcm.Open(nil, data[1:1+ns], data[1+ns:], nil)
	if err != nil {
		return nil, errDecrypt
	}
	return plain, nil
}

// sealJSON = JSON -> gzip -> AES-256-GCM.
func sealJSON(key []byte, v any) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return seal(key, buf.Bytes())
}

func unsealJSON(key, data []byte, v any) error {
	plain, err := unseal(key, data)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return err
	}
	return json.NewDecoder(io.LimitReader(zr, 512<<20)).Decode(v)
}

// ----- backup files: own salt, so they open on any server/account -----

var backupMagic = []byte("QLCBK1")

func backupKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
}

func sealBackup(password string, v any) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	body, err := sealJSON(backupKey(password, salt), v)
	if err != nil {
		return nil, err
	}
	out := append(append([]byte{}, backupMagic...), salt...)
	return append(out, body...), nil
}

func openBackup(password string, data []byte, v any) error {
	if len(data) < len(backupMagic)+16 || !bytes.Equal(data[:len(backupMagic)], backupMagic) {
		return errors.New("đây không phải file sao lưu của Quản lý Chrome")
	}
	salt := data[len(backupMagic) : len(backupMagic)+16]
	if err := unsealJSON(backupKey(password, salt), data[len(backupMagic)+16:], v); err != nil {
		if errors.Is(err, errDecrypt) {
			return errors.New("sai mật khẩu bản sao lưu")
		}
		return err
	}
	return nil
}
