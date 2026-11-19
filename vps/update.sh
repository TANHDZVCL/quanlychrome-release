#!/bin/bash
# Pulls the newest "Quản lý Chrome" release from the private GitHub repo into
# the VPS download folder (/opt/chrome-sync/download). Run by a systemd timer.
set -euo pipefail
REPO="TANHDZVCL/quanlychrome-release"
DL=/opt/chrome-sync/download
TOKEN="$(cat /opt/chrome-sync/github-token)"

get() { # get <path-in-repo> <output-file>
  curl -fsSL --max-time 600 -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github.raw" \
    "https://api.github.com/repos/$REPO/contents/$1?ref=main" -o "$2"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
get release/version.json "$tmp/version.json"
if [ -f "$DL/version.json" ] && cmp -s "$tmp/version.json" "$DL/version.json"; then
  exit 0 # already up to date
fi
get release/ChromeManager.exe "$tmp/ChromeManager.exe"
get release/QuanLyChrome.zip "$tmp/QuanLyChrome.zip"

want="$(sed -n 's/.*"sha256": *"\([0-9a-fA-F]*\)".*/\1/p' "$tmp/version.json")"
got="$(sha256sum "$tmp/ChromeManager.exe" | cut -d' ' -f1)"
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "Bản tải về bị lỗi (sha256 không khớp) — bỏ qua"
  exit 1
fi

mkdir -p "$DL"
for f in ChromeManager.exe QuanLyChrome.zip version.json; do # version.json last
  install -m 644 "$tmp/$f" "$DL/$f.new"
  mv -f "$DL/$f.new" "$DL/$f"
done
echo "Đã cập nhật trang tải lên bản $(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$DL/version.json")"
