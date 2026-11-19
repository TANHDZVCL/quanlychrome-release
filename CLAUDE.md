# Hướng dẫn cho Claude khi làm việc với repo này

Repo riêng tư của người dùng (TANHDZVCL) để phát hành phần mềm "Quản lý Chrome".
Giao tiếp với người dùng bằng tiếng Việt, từng bước đơn giản.

## Cấu trúc
- `source/` — mã Go. Phần mềm Windows ở gốc (`main.go`...), giao diện ở `source/web/`,
  máy chủ đồng bộ `source/cmd/syncserver`, dịch vụ Proxy VPN `source/cmd/proxyd`,
  script cài VPS `source/deploy/install.sh`.
- `release/` — bản đang phát hành. VPS (49.236.211.75, https://49-236-211-75.sslip.io)
  kéo về `/opt/chrome-sync/download` mỗi 10 phút (systemd timer `chrome-sync-update`).

## Phát hành bản mới
1. Sửa code trong `source/`, tăng `appVersion` trong `source/main.go`.
2. `cd source && go vet ./... && GOOS=windows go vet ./... && go test ./...`
3. Build: `GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o ../release/ChromeManager.exe .`
4. Tạo `release/QuanLyChrome.zip` (thư mục `QuanLyChrome/` chứa `ChromeManager.exe` + `HUONG_DAN.txt`).
5. `release/version.json`: `{"version":"X.Y","sha256":"<sha256 của ChromeManager.exe>","notes":"...","file":"ChromeManager.exe"}`
   — ghi version.json SAU CÙNG; sha256 phải khớp, nếu không VPS sẽ từ chối.
6. Commit + push lên `main`. Không cần người dùng làm gì thêm.

Cách nhanh: trong `source/` chạy `./release.sh X.Y "ghi chú"` (sửa đường dẫn REPO trong script nếu cần).

Máy chủ: `release/server/{syncserver,proxyd}` + `release/server.json` (sha256 từng file). VPS
(`vps/update.sh`, timer 5 phút) tự cập nhật chính script, tải server mới khi server.json đổi,
kiểm tra sha256 rồi `systemctl restart chrome-sync` / `chrome-sync-proxyd`.
Riêng `deploy/install.sh` (systemd unit, nginx, docker) KHÔNG tự chạy lại — nếu đổi unit/cờ
khởi động thì người dùng phải chạy lại install.sh.

## Sao lưu VPS
- Timer `chrome-sync-backup` (mỗi 10 phút) chạy `/opt/chrome-sync/backup-to-gdrive.sh` (từ `vps/backup.sh`):
  chỉ khi dữ liệu đổi (tối đa 30 phút/bản), stream `tar` của `data`, `env`, `vpn/proxies.json`
  thẳng lên rclone `gdrive:QuanLyChrome-SaoLuu` (không ghi file trên VPS), giữ 48 bản mới nhất.
  Không sao lưu tài khoản ExpressVPN (`vpn/creds.json`) — khôi phục xong người dùng nhập lại trong mục Proxy VPN.
- Khôi phục (sau khi đã chạy install.sh trên VPS mới):
  `rclone copy gdrive:QuanLyChrome-SaoLuu/<file>.tar.gz /root/ && systemctl stop chrome-sync && tar xzf /root/<file>.tar.gz -C /opt/chrome-sync && chown -R chromesync:chromesync /opt/chrome-sync/data && systemctl start chrome-sync`
