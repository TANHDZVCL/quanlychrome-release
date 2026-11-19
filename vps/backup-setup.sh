#!/bin/bash
# Installs the nightly Google Drive backup (03:00, keeps 14 days).
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "Hãy chạy bằng root"; exit 1; }
DIR=/opt/chrome-sync
REPO="TANHDZVCL/quanlychrome-release"
command -v rclone >/dev/null || { echo "!! Chưa có rclone"; exit 1; }
rclone listremotes | grep -qx 'gdrive:' || { echo "!! rclone chưa có kết nối tên gdrive:"; exit 1; }
curl -fsSL -H "Authorization: Bearer $(cat $DIR/github-token)" -H "Accept: application/vnd.github.raw" \
  "https://api.github.com/repos/$REPO/contents/vps/backup.sh?ref=main" -o "$DIR/backup-to-gdrive.sh"
chmod 700 "$DIR/backup-to-gdrive.sh"
cat > /etc/systemd/system/chrome-sync-backup.service <<UNIT
[Unit]
Description=Quan ly Chrome - sao luu len Google Drive
After=network-online.target

[Service]
Type=oneshot
ExecStart=$DIR/backup-to-gdrive.sh
UNIT
cat > /etc/systemd/system/chrome-sync-backup.timer <<UNIT
[Unit]
Description=Quan ly Chrome - sao luu moi dem

[Timer]
OnCalendar=*-*-* 03:00:00
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now chrome-sync-backup.timer >/dev/null
echo "==> Sao lưu thử lần đầu..."
"$DIR/backup-to-gdrive.sh"
echo
echo "==================== XONG ===================="
echo " Mỗi đêm ~3 giờ VPS tự sao lưu lên Google Drive,"
echo " thư mục QuanLyChrome-SaoLuu, giữ 14 ngày gần nhất."
echo " Các bản đang có:"
rclone lsl gdrive:QuanLyChrome-SaoLuu | sort -k2 | tail -5
echo "=============================================="
