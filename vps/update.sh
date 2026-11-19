#!/bin/bash
# Pulls the newest "Quản lý Chrome" release from the private GitHub repo:
#  - the Windows app into the download folder (/opt/chrome-sync/download),
#  - the server programs (syncserver, proxyd) when they changed,
#  - and this script itself.
# Run every few minutes by the chrome-sync-update timer.
set -euo pipefail
REPO="TANHDZVCL/quanlychrome-release"
DIR=/opt/chrome-sync
DL=$DIR/download
SELF=$DIR/update-from-github.sh
TOKEN="$(cat $DIR/github-token)"

get() { # get <path-in-repo> <output-file>
  curl -fsSL --max-time 600 -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github.raw" \
    "https://api.github.com/repos/$REPO/contents/$1?ref=main" -o "$2"
}
jget() { sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" "$2"; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1) this script
if get vps/update.sh "$tmp/update.sh" && ! cmp -s "$tmp/update.sh" "$SELF"; then
  install -m 700 "$tmp/update.sh" "$SELF.new" && mv -f "$SELF.new" "$SELF"
  echo "Đã cập nhật script cập nhật, chạy lại..."
  rm -rf "$tmp"
  exec "$SELF"
fi

# 2) Windows app
get release/version.json "$tmp/version.json"
if ! { [ -f "$DL/version.json" ] && cmp -s "$tmp/version.json" "$DL/version.json"; }; then
  get release/ChromeManager.exe "$tmp/ChromeManager.exe"
  get release/QuanLyChrome.zip "$tmp/QuanLyChrome.zip"
  if [ "$(jget sha256 "$tmp/version.json")" = "$(sha256sum "$tmp/ChromeManager.exe" | cut -d' ' -f1)" ]; then
    mkdir -p "$DL"
    for f in ChromeManager.exe QuanLyChrome.zip version.json; do # version.json last
      install -m 644 "$tmp/$f" "$DL/$f.new" && mv -f "$DL/$f.new" "$DL/$f"
    done
    echo "Đã cập nhật trang tải lên bản $(jget version "$DL/version.json")"
  else
    echo "!! Bản phần mềm tải về bị lỗi (sha256 không khớp) — bỏ qua"
  fi
fi

# 3) server programs
if get release/server.json "$tmp/server.json" 2>/dev/null && ! cmp -s "$tmp/server.json" "$DIR/server.json" 2>/dev/null; then
  ok=1
  for b in syncserver proxyd; do
    get "release/server/$b" "$tmp/$b" || { ok=0; break; }
    [ "$(jget "$b" "$tmp/server.json")" = "$(sha256sum "$tmp/$b" | cut -d' ' -f1)" ] || { ok=0; break; }
  done
  if [ "$ok" = 1 ]; then
    for b in syncserver proxyd; do
      if ! cmp -s "$tmp/$b" "$DIR/$b" 2>/dev/null; then
        install -m 755 "$tmp/$b" "$DIR/$b.new" && mv -f "$DIR/$b.new" "$DIR/$b"
        case "$b" in
          syncserver) systemctl restart chrome-sync ;;
          proxyd) systemctl is-enabled chrome-sync-proxyd >/dev/null 2>&1 && systemctl restart chrome-sync-proxyd ;;
        esac
        echo "Đã cập nhật $b"
      fi
    done
    install -m 644 "$tmp/server.json" "$DIR/server.json"
    echo "Máy chủ đang ở bản $(jget version "$DIR/server.json")"
  else
    echo "!! Bản máy chủ tải về bị lỗi (sha256 không khớp) — bỏ qua"
  fi
fi
