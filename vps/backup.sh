#!/bin/bash
# Nightly backup of the sync server data to Google Drive (rclone remote "gdrive:").
# The profile/cookie data is already encrypted with the user's password.
set -euo pipefail
DIR=/opt/chrome-sync
REMOTE="gdrive:QuanLyChrome-SaoLuu"
KEEP_DAYS=14

ts="$(date +%Y%m%d-%H%M)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
items=(data env)
[ -f "$DIR/vpn/proxies.json" ] && items+=(vpn/proxies.json) # proxy list (not the ExpressVPN login)
tar czf "$tmp/chrome-sync-$ts.tar.gz" -C "$DIR" "${items[@]}"
rclone copy "$tmp/chrome-sync-$ts.tar.gz" "$REMOTE"
rclone delete "$REMOTE" --min-age "${KEEP_DAYS}d" --include 'chrome-sync-*.tar.gz' || true
echo "Đã sao lưu chrome-sync-$ts.tar.gz ($(du -h "$tmp/chrome-sync-$ts.tar.gz" | cut -f1)) lên Google Drive/QuanLyChrome-SaoLuu"
