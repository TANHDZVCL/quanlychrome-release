#!/bin/bash
# Installs the Google Drive backup: checks every 10 min, backs up only when data changed.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "Hãy chạy bằng root"; exit 1; }
DIR=/opt/chrome-sync
REPO="TANHDZVCL/quanlychrome-release"
command -v rclone >/dev/null || { echo "!! Chưa có rclone"; exit 1; }
rclone listremotes | grep -qx 'gdrive:' || { echo "!! rclone chưa có kết nối tên gdrive:"; exit 1; }
curl -fsSL -H "Authorization: Bearer $(cat $DIR/github-token)" -H "Accept: application/vnd.github.raw" \
  "https://api.github.com/repos/$REPO/contents/vps/backup.sh?ref=main" -o "$DIR/backup-to-gdrive.sh" --force
chmod 700 "$DIR/backup-to-gdrive.sh" --force
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
Description=Quan ly Chrome - kiem tra va sao luu khi co thay doi

[Timer]
OnBootSec=5min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now chrome-sync-backup.timer >/dev/null
echo "==> Sao lưu thử lần đầu..."
"$DIR/backup-to-gdrive.sh" --force
echo
echo "==================== XONG ===================="
echo " VPS kiểm tra mỗi 10 phút, chỉ sao lưu khi dữ liệu thay đổi"
echo " (tối đa 30 phút/bản), đẩy thẳng lên Google Drive, không lưu trên VPS."
echo " Thư mục QuanLyChrome-SaoLuu, giữ 48 bản mới nhất."
echo " Các bản đang có:"
rclone lsl gdrive:QuanLyChrome-SaoLuu | sort -k2 | tail -5
echo "=============================================="
