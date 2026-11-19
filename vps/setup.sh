#!/bin/bash
# One-time setup on the VPS: store a read-only GitHub token and install a
# timer that pulls new releases every 10 minutes.
# Usage (as root):  TOKEN=github_pat_... bash setup.sh
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "Hãy chạy bằng root"; exit 1; }
[ -n "${TOKEN:-}" ] || { echo "Thiếu TOKEN"; exit 1; }
DIR=/opt/chrome-sync
REPO="TANHDZVCL/quanlychrome-release"
mkdir -p "$DIR/download"
umask 077
printf '%s' "$TOKEN" > "$DIR/github-token"
chmod 600 "$DIR/github-token"
curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github.raw" \
  "https://api.github.com/repos/$REPO/contents/vps/update.sh?ref=main" -o "$DIR/update-from-github.sh"
chmod 700 "$DIR/update-from-github.sh"

cat > /etc/systemd/system/chrome-sync-update.service <<UNIT
[Unit]
Description=Quan ly Chrome - lay ban moi tu GitHub
After=network-online.target

[Service]
Type=oneshot
ExecStart=$DIR/update-from-github.sh
UNIT
cat > /etc/systemd/system/chrome-sync-update.timer <<UNIT
[Unit]
Description=Quan ly Chrome - kiem tra ban moi moi 10 phut

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now chrome-sync-update.timer >/dev/null
if "$DIR/update-from-github.sh"; then
  echo
  echo "==================== XONG ===================="
  echo " VPS sẽ tự lấy bản mới từ GitHub mỗi 10 phút."
  echo " Bản hiện tại: $(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' $DIR/download/version.json)"
  echo "=============================================="
else
  echo "!! Không tải được từ GitHub — kiểm tra lại token (quyền Contents: Read-only, đúng repo)."
  exit 1
fi
