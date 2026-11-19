#!/bin/bash
# Backs up the sync server data to Google Drive (rclone remote "gdrive:")
# only when it changed, at most every 30 minutes. Nothing is written to the
# VPS disk: tar is streamed straight to Drive. Run every 10 min by a timer.
# The profile/cookie data is already encrypted with the user's password.
# Usage: backup.sh [--force]
set -euo pipefail
DIR=/opt/chrome-sync
REMOTE="gdrive:QuanLyChrome-SaoLuu"
KEEP=48          # newest backups kept on Drive
MIN_GAP=1800     # seconds between two backups
STATE="$DIR/.backup-state"
FORCE="${1:-}"

cd "$DIR"
items=(data env)
[ -f vpn/proxies.json ] && items+=(vpn/proxies.json) # proxy list (not the ExpressVPN login)

fp="$(find "${items[@]}" -type f -printf '%P %s %T@\n' 2>/dev/null | sort | sha256sum | cut -c1-64)"
last_fp=""; last_ts=0
[ -f "$STATE" ] && read -r last_fp last_ts < "$STATE" || true
now="$(date +%s)"
if [ "$FORCE" != "--force" ]; then
  [ "$fp" = "$last_fp" ] && exit 0                      # nothing changed
  [ $((now - last_ts)) -lt $MIN_GAP ] && exit 0          # too soon, next run will do it
fi

name="chrome-sync-$(date +%Y%m%d-%H%M).tar.gz"
set +o pipefail
tar czf - --warning=no-file-changed --ignore-failed-read "${items[@]}" | rclone rcat "$REMOTE/$name"
rc=${PIPESTATUS[1]}
set -o pipefail
if [ "$rc" -ne 0 ]; then
  echo "!! Không đẩy được lên Google Drive (rclone lỗi $rc)"
  exit 1
fi
echo "$fp $now" > "$STATE"

# keep only the newest $KEEP backups
rclone lsf "$REMOTE" --include 'chrome-sync-*.tar.gz' | sort | head -n -"$KEEP" | while read -r f; do
  rclone deletefile "$REMOTE/$f" || true
done
echo "Đã sao lưu $name lên Google Drive/QuanLyChrome-SaoLuu"
