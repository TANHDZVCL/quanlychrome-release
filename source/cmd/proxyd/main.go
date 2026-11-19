// proxyd turns an ExpressVPN subscription into several HTTP proxies on this
// VPS: one gluetun Docker container per proxy, each connected to a chosen
// country and exposing an authenticated HTTP proxy on its own port.
//
// It runs as root (it drives Docker) and listens on localhost only. The sync
// server forwards requests from the logged-in admin, with a shared key.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	image     = "qmcgaw/gluetun"
	firstPort = 20001
	lastPort  = 20030
	maxProxy  = 12
	prefix    = "cmpx-"
)

// ExpressVPN locations known to gluetun.
var countries = []string{"Vietnam", "Singapore", "Japan", "Hong Kong", "Thailand", "South Korea", "Taiwan",
	"Malaysia", "Indonesia", "Philippines (via Singapore)", "Cambodia", "Laos", "Myanmar", "Macau", "India (via Singapore)",
	"USA", "Canada", "Mexico", "UK", "Germany", "France", "Netherlands", "Spain", "Italy", "Switzerland", "Sweden",
	"Norway", "Denmark", "Finland", "Ireland", "Belgium", "Austria", "Poland", "Czech Republic", "Romania",
	"Turkey", "Australia", "New Zealand", "Brazil", "Argentina", "Chile", "Colombia", "Peru", "South Africa",
	"Egypt", "Israel", "Albania", "Algeria", "Andorra", "Azerbaijan", "Bahamas", "Bangladesh", "Belarus", "Bermuda",
	"Bhutan", "Bolivia", "Brunei", "Bulgaria", "Cayman Islands", "Costa Rica", "Croatia", "Cuba", "Cyprus",
	"Dominican Republic", "Ecuador", "Estonia", "Georgia", "Ghana", "Greece", "Guam", "Guatemala", "Honduras",
	"Hungary", "Iceland", "India (via UK)", "Jamaica", "Kazakhstan", "Latvia", "Liechtenstein", "Lithuania",
	"Luxembourg", "Moldova", "Mongolia", "Morocco", "Nepal", "North Macedonia", "Panama", "Portugal",
	"Puerto Rico", "Serbia", "Slovakia", "Slovenia", "Sri Lanka", "Trinidad and Tobago", "Ukraine", "Uruguay", "Venezuela"}

var reID = regexp.MustCompile(`^[a-f0-9]{8}$`)

type proxy struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Country   string    `json:"country"`
	Port      int       `json:"port"`
	User      string    `json:"user"`
	Pass      string    `json:"pass"`
	Created   time.Time `json:"created"`
	IP        string    `json:"ip,omitempty"`
	City      string    `json:"city,omitempty"`
	IPCountry string    `json:"ipCountry,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Status    string    `json:"status,omitempty"` // filled from docker at read time
}

type creds struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

type daemon struct {
	dir string
	key string

	mu      sync.Mutex
	proxies []*proxy
	creds   creds
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8791", "listen address (keep it on localhost)")
	dir := flag.String("dir", "/opt/chrome-sync/vpn", "state directory")
	keyFile := flag.String("key", "/opt/chrome-sync/proxyd.key", "shared key file")
	flag.Parse()

	key, err := os.ReadFile(*keyFile)
	if err != nil || len(strings.TrimSpace(string(key))) < 16 {
		log.Fatalf("missing key file %s", *keyFile)
	}
	d := &daemon{dir: *dir, key: strings.TrimSpace(string(key))}
	if err := os.MkdirAll(filepath.Join(d.dir, "env"), 0o700); err != nil {
		log.Fatal(err)
	}
	_ = readJSON(filepath.Join(d.dir, "proxies.json"), &d.proxies)
	_ = readJSON(filepath.Join(d.dir, "creds.json"), &d.creds)
	log.Printf("proxyd on %s, %d proxies", *addr, len(d.proxies))
	srv := &http.Server{Addr: *addr, Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d *daemon) saveLocked() error {
	return writeJSON(filepath.Join(d.dir, "proxies.json"), d.proxies)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", d.status)
	mux.HandleFunc("PUT /creds", d.setCreds)
	mux.HandleFunc("POST /proxies", d.create)
	mux.HandleFunc("DELETE /proxies/{id}", d.remove)
	mux.HandleFunc("POST /proxies/{id}/restart", d.restart)
	mux.HandleFunc("POST /proxies/{id}/check", d.check)
	mux.HandleFunc("GET /proxies/{id}/logs", d.logs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Proxyd-Key")), []byte(d.key)) != 1 {
			fail(w, 403, "forbidden")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// ---------- docker ----------

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, errors.New(s)
	}
	return s, nil
}

func dockerOK() bool {
	_, err := docker("info", "--format", "{{.ServerVersion}}")
	return err == nil
}

func containerStates() map[string]string {
	out, err := docker("ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}|{{.Status}}")
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(out, "\n") {
		name, st, ok := strings.Cut(line, "|")
		if ok {
			m[strings.TrimPrefix(name, prefix)] = st
		}
	}
	return m
}

func (d *daemon) envFile(p *proxy) (string, error) {
	if d.creds.User == "" {
		return "", errors.New("chưa nhập tài khoản ExpressVPN")
	}
	lines := []string{
		"VPN_SERVICE_PROVIDER=expressvpn",
		"VPN_TYPE=openvpn",
		"OPENVPN_USER=" + d.creds.User,
		"OPENVPN_PASSWORD=" + d.creds.Pass,
		"SERVER_COUNTRIES=" + p.Country,
		"HTTPPROXY=on",
		"HTTPPROXY_USER=" + p.User,
		"HTTPPROXY_PASSWORD=" + p.Pass,
		"HTTPPROXY_STEALTH=on",
		"TZ=Asia/Ho_Chi_Minh",
	}
	f := filepath.Join(d.dir, "env", p.ID+".env")
	return f, os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func (d *daemon) start(p *proxy) error {
	env, err := d.envFile(p)
	if err != nil {
		return err
	}
	_, _ = docker("rm", "-f", prefix+p.ID)
	_, err = docker("run", "-d", "--name", prefix+p.ID, "--restart", "unless-stopped",
		"--cap-add=NET_ADMIN", "--device", "/dev/net/tun:/dev/net/tun",
		"-p", fmt.Sprintf("%d:8888/tcp", p.Port), "--env-file", env,
		"--log-opt", "max-size=1m", "--log-opt", "max-file=2", image)
	return err
}

// ---------- handlers ----------

func (d *daemon) status(w http.ResponseWriter, r *http.Request) {
	_, tunErr := os.Stat("/dev/net/tun")
	ok := dockerOK()
	states := map[string]string{}
	if ok {
		states = containerStates()
	}
	d.mu.Lock()
	list := make([]proxy, 0, len(d.proxies))
	for _, p := range d.proxies {
		cp := *p
		cp.Status = states[p.ID]
		if cp.Status == "" {
			cp.Status = "không chạy"
		}
		list = append(list, cp)
	}
	credsSet := d.creds.User != ""
	credUser := d.creds.User
	d.mu.Unlock()
	if len(credUser) > 4 {
		credUser = credUser[:4] + "…"
	}
	reply(w, 200, map[string]any{
		"docker": ok, "tun": tunErr == nil, "credsSet": credsSet, "credUser": credUser,
		"proxies": list, "countries": countries, "max": maxProxy,
	})
}

func (d *daemon) setCreds(w http.ResponseWriter, r *http.Request) {
	var c creds
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&c); err != nil {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	c.User, c.Pass = strings.TrimSpace(c.User), strings.TrimSpace(c.Pass)
	if c.User == "" || c.Pass == "" || strings.ContainsAny(c.User+c.Pass, "\r\n") || len(c.User)+len(c.Pass) > 400 {
		fail(w, 400, "Tên đăng nhập / mật khẩu ExpressVPN không hợp lệ")
		return
	}
	d.mu.Lock()
	d.creds = c
	err := writeJSON(filepath.Join(d.dir, "creds.json"), c)
	ps := slices.Clone(d.proxies)
	d.mu.Unlock()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// Restart existing proxies with the new account.
	var errs []string
	for _, p := range ps {
		if err := d.start(p); err != nil {
			errs = append(errs, p.Name+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		fail(w, 500, strings.Join(errs, "\n"))
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (d *daemon) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Country string `json:"country"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || !slices.Contains(countries, in.Country) {
		fail(w, 400, "Quốc gia không hợp lệ")
		return
	}
	if !dockerOK() {
		fail(w, 500, "Docker chưa chạy trên VPS — hãy chạy lại install.sh")
		return
	}
	d.mu.Lock()
	if len(d.proxies) >= maxProxy {
		d.mu.Unlock()
		fail(w, 400, fmt.Sprintf("Tối đa %d proxy (ExpressVPN giới hạn số thiết bị kết nối cùng lúc)", maxProxy))
		return
	}
	used := map[int]bool{}
	for _, p := range d.proxies {
		used[p.Port] = true
	}
	port := 0
	for pt := firstPort; pt <= lastPort; pt++ {
		if !used[pt] {
			port = pt
			break
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.Country
	}
	if len(name) > 60 {
		name = name[:60]
	}
	p := &proxy{ID: randHex(4), Name: name, Country: in.Country, Port: port, User: "u" + randHex(4), Pass: randHex(10), Created: time.Now()}
	d.mu.Unlock()
	if port == 0 {
		fail(w, 400, "Hết cổng trống")
		return
	}
	d.mu.Lock()
	err := d.start(p)
	if err == nil {
		d.proxies = append(d.proxies, p)
		err = d.saveLocked()
	}
	d.mu.Unlock()
	if err != nil {
		_, _ = docker("rm", "-f", prefix+p.ID)
		fail(w, 500, "Không tạo được proxy: "+err.Error())
		return
	}
	log.Printf("created proxy %s %s port %d", p.ID, p.Country, p.Port)
	reply(w, 200, p)
}

func (d *daemon) find(id string) *proxy {
	for _, p := range d.proxies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (d *daemon) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reID.MatchString(id) {
		fail(w, 400, "id không hợp lệ")
		return
	}
	_, _ = docker("rm", "-f", prefix+id)
	d.mu.Lock()
	d.proxies = slices.DeleteFunc(d.proxies, func(p *proxy) bool { return p.ID == id })
	err := d.saveLocked()
	d.mu.Unlock()
	_ = os.Remove(filepath.Join(d.dir, "env", id+".env"))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

// restart reconnects the VPN, which usually gives a new IP in the same country.
func (d *daemon) restart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.Lock()
	p := d.find(id)
	d.mu.Unlock()
	if p == nil {
		fail(w, 404, "không tìm thấy proxy")
		return
	}
	if _, err := docker("restart", prefix+id); err != nil {
		d.mu.Lock()
		err = d.start(p)
		d.mu.Unlock()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (d *daemon) check(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.Lock()
	p := d.find(id)
	var cp proxy
	if p != nil {
		cp = *p
	}
	d.mu.Unlock()
	if p == nil {
		fail(w, 404, "không tìm thấy proxy")
		return
	}
	pu := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", cp.Port), User: url.UserPassword(cp.User, cp.Pass)}
	c := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	start := time.Now()
	resp, err := c.Get("https://ipinfo.io/json")
	if err != nil {
		fail(w, 502, "Proxy chưa sẵn sàng (VPN đang kết nối hoặc lỗi). Xem Nhật ký. ("+err.Error()+")")
		return
	}
	defer resp.Body.Close()
	var info struct{ IP, City, Country string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil || info.IP == "" {
		fail(w, 502, "Không đọc được IP qua proxy")
		return
	}
	d.mu.Lock()
	if p := d.find(id); p != nil {
		p.IP, p.City, p.IPCountry, p.CheckedAt = info.IP, info.City, info.Country, time.Now()
		_ = d.saveLocked()
	}
	d.mu.Unlock()
	reply(w, 200, map[string]any{"ip": info.IP, "city": info.City, "country": info.Country, "ms": time.Since(start).Milliseconds()})
}

var reSecret = regexp.MustCompile(`(?i)(password|user)=\S+`)

func (d *daemon) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reID.MatchString(id) {
		fail(w, 400, "id không hợp lệ")
		return
	}
	out, _ := docker("logs", "--tail", "40", prefix+id)
	reply(w, 200, map[string]string{"logs": reSecret.ReplaceAllString(out, "$1=***")})
}
