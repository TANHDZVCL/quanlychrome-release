#!/bin/bash
# Build and publish a new app version to the private release repo.
# Usage: ./release.sh <version> "<notes>"
set -euo pipefail
VER="$1"; NOTES="$2"
SRC="$(cd "$(dirname "$0")" && pwd)"
REPO=/home/user/quanlychrome-release
cd "$SRC"
sed -i "s/^const appVersion = \".*\"/const appVersion = \"$VER\"/" main.go
gofmt -l . | (! grep .) || { echo "gofmt needed"; exit 1; }
go vet ./... && GOOS=windows go vet ./...
NO_PROXY='*' HTTPS_PROXY= HTTP_PROXY= https_proxy= http_proxy= go test -count=1 ./... >/dev/null
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o "$REPO/release/ChromeManager.exe" .
tmp="$(mktemp -d)"; mkdir "$tmp/QuanLyChrome"
cp "$REPO/release/ChromeManager.exe" HUONG_DAN.txt "$tmp/QuanLyChrome/"
rm -f "$REPO/release/QuanLyChrome.zip"
(cd "$tmp" && python3 -c "
import zipfile,os
with zipfile.ZipFile('$REPO/release/QuanLyChrome.zip','w',zipfile.ZIP_DEFLATED) as z:
  for f in os.listdir('QuanLyChrome'): z.write('QuanLyChrome/'+f)
")
rm -rf "$tmp"
SHA="$(sha256sum "$REPO/release/ChromeManager.exe" | cut -d' ' -f1)"
python3 -c "
import json,sys
json.dump({'version':sys.argv[1],'sha256':sys.argv[2],'notes':sys.argv[3],'file':'ChromeManager.exe'}, open(sys.argv[4],'w'), ensure_ascii=False)
" "$VER" "$SHA" "$NOTES" "$REPO/release/version.json"
# server programs: published only when they changed
mkdir -p "$REPO/release/server"
for b in syncserver proxyd; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$REPO/release/server/$b.tmp" "./cmd/$b"
done
S1="$(sha256sum "$REPO/release/server/syncserver.tmp" | cut -d' ' -f1)"
S2="$(sha256sum "$REPO/release/server/proxyd.tmp" | cut -d' ' -f1)"
if [ -f "$REPO/release/server.json" ] && grep -q "$S1" "$REPO/release/server.json" && grep -q "$S2" "$REPO/release/server.json"; then
  rm -f "$REPO/release/server/"*.tmp
else
  mv -f "$REPO/release/server/syncserver.tmp" "$REPO/release/server/syncserver"
  mv -f "$REPO/release/server/proxyd.tmp" "$REPO/release/server/proxyd"
  printf '{"version": "%s", "syncserver": "%s", "proxyd": "%s"}\n' "$VER" "$S1" "$S2" > "$REPO/release/server.json"
  echo "Máy chủ có thay đổi -> phát hành server $VER"
fi
# sync source (without build outputs)
rm -rf "$REPO/source" && mkdir -p "$REPO/source"
tar cf - --exclude=./dist --exclude=./build --exclude='*.zip' \
  --exclude=./deploy/download/ChromeManager.exe --exclude=./deploy/download/QuanLyChrome.zip \
  --exclude=./deploy/download/version.json . | tar xf - -C "$REPO/source"
cd "$REPO"
git add -A
git -c user.name="Claude" -c user.email="noreply@anthropic.com" commit -q -m "Phát hành bản $VER: $NOTES

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01CxWxvcJKTnRJANetLGakEh"
for i in 1 2 3 4; do git -c push.negotiate=true push -q origin main && break; sleep $((i*2)); done
git log --oneline -1
cat release/version.json; echo
