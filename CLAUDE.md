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

Thay đổi máy chủ (`cmd/syncserver`, `cmd/proxyd`, `deploy/install.sh`) KHÔNG tự cập nhật:
người dùng phải chạy lại `install.sh` trên VPS.

## Sao lưu VPS
- Timer `chrome-sync-backup` (03:00 mỗi đêm) chạy `/opt/chrome-sync/backup-to-gdrive.sh` (từ `vps/backup.sh`):
  nén `data`, `env`, `vpn/proxies.json` → rclone `gdrive:QuanLyChrome-SaoLuu`, giữ 14 ngày.
  Không sao lưu tài khoản ExpressVPN (`vpn/creds.json`) — khôi phục xong người dùng nhập lại trong mục Proxy VPN.
- Khôi phục (sau khi đã chạy install.sh trên VPS mới):
  `rclone copy gdrive:QuanLyChrome-SaoLuu/<file>.tar.gz /root/ && systemctl stop chrome-sync && tar xzf /root/<file>.tar.gz -C /opt/chrome-sync && chown -R chromesync:chromesync /opt/chrome-sync/data && systemctl start chrome-sync`
