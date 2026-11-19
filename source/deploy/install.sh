#!/bin/bash
# Cài / cập nhật máy chủ đồng bộ "Quản lý Chrome" trên Ubuntu.
# Cách dùng (chạy bằng root, trong thư mục có file syncserver):
#   bash install.sh sync.nguyentuananh.site
set -euo pipefail

DOMAIN="${1:-sync.nguyentuananh.site}"
PORT=8790
DIR=/opt/chrome-sync
HERE="$(cd "$(dirname "$0")" && pwd)"

[ "$(id -u)" = 0 ] || { echo "Hãy chạy bằng root"; exit 1; }
[ -f "$HERE/syncserver" ] || { echo "Không thấy file syncserver cạnh install.sh"; exit 1; }

echo "==> Cài chương trình vào $DIR"
id chromesync &>/dev/null || useradd --system --home-dir "$DIR" --shell /usr/sbin/nologin chromesync
mkdir -p "$DIR/data" "$DIR/download"
if [ -d "$HERE/download" ]; then cp -f "$HERE"/download/* "$DIR/download/"; fi
chmod -R a+rX "$DIR/download"
install -m 755 "$HERE/syncserver" "$DIR/syncserver.new"
mv -f "$DIR/syncserver.new" "$DIR/syncserver"
if [ -f "$HERE/proxyd" ]; then
  install -m 755 "$HERE/proxyd" "$DIR/proxyd.new"
  mv -f "$DIR/proxyd.new" "$DIR/proxyd"
fi
if [ ! -f "$DIR/proxyd.key" ]; then
  tr -dc 'a-f0-9' </dev/urandom | head -c 40 > "$DIR/proxyd.key" || true
fi
chown root:chromesync "$DIR/proxyd.key"
chmod 640 "$DIR/proxyd.key"
if [ ! -f "$DIR/env" ]; then
  CODE="$(tr -dc 'A-HJ-NP-Za-km-z2-9' </dev/urandom | head -c 12 || true)"
  echo "SYNC_REGISTER_CODE=$CODE" > "$DIR/env"
fi
chmod 600 "$DIR/env"
chown -R chromesync:chromesync "$DIR/data"
chmod 700 "$DIR/data"

cat > /etc/systemd/system/chrome-sync.service <<UNIT
[Unit]
Description=Quan ly Chrome - may chu dong bo
After=network.target

[Service]
User=chromesync
EnvironmentFile=$DIR/env
ExecStart=$DIR/syncserver -addr 127.0.0.1:$PORT -data $DIR/data -downloads $DIR/download -proxyd http://127.0.0.1:8791 -proxyd-key $DIR/proxyd.key
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DIR/data

[Install]
WantedBy=multi-user.target
UNIT
if [ -x "$DIR/proxyd" ]; then
  echo "==> Cài Docker + công cụ VPN cho tính năng Proxy VPN (lần đầu mất vài phút)"
  if ! command -v docker >/dev/null; then
    apt-get update -qq && apt-get install -y -qq docker.io
  fi
  systemctl enable --now docker >/dev/null 2>&1 || true
  if [ ! -e /dev/net/tun ]; then
    echo "!! VPS không có /dev/net/tun — tính năng Proxy VPN sẽ không chạy được trên VPS này."
  fi
  docker pull -q qmcgaw/gluetun >/dev/null && echo "    Đã tải công cụ VPN (gluetun)"
  mkdir -p "$DIR/vpn" && chmod 700 "$DIR/vpn"
  cat > /etc/systemd/system/chrome-sync-proxyd.service <<UNIT2
[Unit]
Description=Quan ly Chrome - quan ly proxy ExpressVPN
After=network.target docker.service
Requires=docker.service

[Service]
ExecStart=$DIR/proxyd -addr 127.0.0.1:8791 -dir $DIR/vpn -key $DIR/proxyd.key
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT2
fi
systemctl daemon-reload
if [ -x "$DIR/proxyd" ]; then
  systemctl enable --now chrome-sync-proxyd
  systemctl restart chrome-sync-proxyd
fi
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow 20001:20030/tcp >/dev/null || true
fi
systemctl enable --now chrome-sync
systemctl restart chrome-sync
sleep 1
curl -fsS "http://127.0.0.1:$PORT/api/ping" >/dev/null && echo "    Máy chủ đồng bộ đang chạy (cổng nội bộ $PORT)"

echo "==> Cấu hình nginx cho $DOMAIN"
if ! command -v nginx >/dev/null; then
  if ss -ltn '( sport = :80 or sport = :443 )' | grep -q LISTEN; then
    echo "!! Cổng 80/443 đang do chương trình khác (không phải nginx) dùng."
    echo "!! Dừng lại để không ảnh hưởng website hiện có. Hãy cấu hình reverse proxy tới 127.0.0.1:$PORT thủ công."
    exit 1
  fi
  apt-get update -qq && apt-get install -y -qq nginx
fi
SITE=/etc/nginx/sites-available/chrome-sync
cat > "$SITE" <<NGINX
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;
    client_max_body_size 70m;
    location / {
        proxy_pass http://127.0.0.1:$PORT;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_read_timeout 120s;
    }
}
NGINX
ln -sf "$SITE" /etc/nginx/sites-enabled/chrome-sync
if ! nginx -t; then
  echo "!! Cấu hình nginx lỗi, gỡ bỏ phần vừa thêm để không ảnh hưởng website khác"
  rm -f /etc/nginx/sites-enabled/chrome-sync
  exit 1
fi
systemctl reload nginx
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow 'Nginx Full' >/dev/null || true
fi

echo "==> Lấy chứng chỉ HTTPS (Let's Encrypt)"
if ! command -v certbot >/dev/null; then
  apt-get install -y -qq certbot python3-certbot-nginx
fi
if certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos --register-unsafely-without-email --redirect; then
  echo "    HTTPS OK"
else
  echo "!! Chưa lấy được HTTPS. Thường do tên miền $DOMAIN chưa trỏ về IP VPS này."
  echo "!! Đợi DNS cập nhật (5–30 phút) rồi chạy lại: bash install.sh $DOMAIN"
  exit 1
fi

sleep 1
if curl -fsS "https://$DOMAIN/api/ping" >/dev/null; then
  echo
  echo "==================== XONG ===================="
  echo " Địa chỉ máy chủ : https://$DOMAIN"
  echo " Trang tải app   : https://$DOMAIN/tai/"
  echo " Mã đăng ký      : $(sed 's/^SYNC_REGISTER_CODE=//' "$DIR/env")"
  echo " Dữ liệu         : $DIR/data   (sao lưu thư mục này)"
  echo "=============================================="
else
  echo "!! Không truy cập được https://$DOMAIN/api/ping — kiểm tra DNS/tường lửa."
fi
