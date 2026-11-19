package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// Chrome cannot take a username/password for its proxy on the command line.
// For proxies with auth we run a tiny local HTTP proxy (127.0.0.1, random port)
// that Chrome talks to without auth, and it forwards to the real upstream
// proxy with the credentials attached.

func (p Proxy) URL() *url.URL {
	u := &url.URL{Scheme: p.Type, Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.HasAuth() {
		u.User = url.UserPassword(p.User, p.Pass)
	}
	return u
}

// dialThrough opens a TCP connection to target ("host:port") through the proxy.
func dialThrough(ctx context.Context, p Proxy, target string) (net.Conn, error) {
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	switch p.Type {
	case "socks5":
		var auth *xproxy.Auth
		if p.HasAuth() {
			auth = &xproxy.Auth{User: p.User, Password: p.Pass}
		}
		sd, err := xproxy.SOCKS5("tcp", addr, auth, d)
		if err != nil {
			return nil, err
		}
		return sd.(xproxy.ContextDialer).DialContext(ctx, "tcp", target)
	case "http":
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(dl)
		} else {
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		}
		req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
		if p.HasAuth() {
			req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.User+":"+p.Pass)) + "\r\n"
		}
		req += "\r\n"
		if _, err := io.WriteString(c, req); err != nil {
			c.Close()
			return nil, err
		}
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			c.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			c.Close()
			if resp.StatusCode == http.StatusProxyAuthRequired {
				return nil, errors.New("proxy từ chối: sai tài khoản/mật khẩu (407)")
			}
			return nil, fmt.Errorf("proxy từ chối: %s", resp.Status)
		}
		_ = c.SetDeadline(time.Time{})
		if br.Buffered() > 0 {
			return &bufConn{Conn: c, r: br}, nil
		}
		return c, nil
	}
	return nil, fmt.Errorf("loại proxy không hỗ trợ: %q", p.Type)
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Forwarder is the local auth-less proxy Chrome connects to.
type Forwarder struct {
	up   Proxy
	ln   net.Listener
	srv  *http.Server
	tr   *http.Transport
	once sync.Once
}

// StartForwarder listens on 127.0.0.1:port (port 0 = any free port).
func StartForwarder(up Proxy, port int) (*Forwarder, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil && port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	f := &Forwarder{up: up, ln: ln}
	f.tr = &http.Transport{
		Proxy:              http.ProxyURL(up.URL()),
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		MaxIdleConns:       50,
		IdleConnTimeout:    60 * time.Second,
	}
	f.srv = &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

func (f *Forwarder) Addr() string { return f.ln.Addr().String() }
func (f *Forwarder) Port() int    { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *Forwarder) Close() {
	f.once.Do(func() {
		_ = f.srv.Close()
		_ = f.ln.Close() // Serve may not have registered the listener yet
		f.tr.CloseIdleConnections()
	})
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "chỉ dùng làm proxy", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := f.tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "Lỗi proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (f *Forwarder) handleConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	upConn, err := dialThrough(ctx, f.up, r.Host)
	cancel()
	if err != nil {
		http.Error(w, "Lỗi proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upConn.Close()
		http.Error(w, "hijack không hỗ trợ", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upConn.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		upConn.Close()
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		if _, err := upConn.Write(b); err != nil {
			client.Close()
			upConn.Close()
			return
		}
	}
	pipe(client, upConn)
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// CheckProxy fetches the public IP seen through the proxy.
type ProxyCheck struct {
	OK      bool   `json:"ok"`
	IP      string `json:"ip,omitempty"`
	Country string `json:"country,omitempty"`
	City    string `json:"city,omitempty"`
	Ms      int64  `json:"ms"`
	Error   string `json:"error,omitempty"`
}

func CheckProxy(p Proxy) ProxyCheck {
	start := time.Now()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !p.Enabled() {
				return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
			}
			return dialThrough(ctx, p, addr)
		},
		Proxy: nil,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	var info struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Country string `json:"country"`
	}
	err := getJSON(client, "https://ipinfo.io/json", &info)
	if err != nil || info.IP == "" {
		// Fallback service.
		info.City, info.Country = "", ""
		err = getJSON(client, "https://api.ipify.org?format=json", &info)
	}
	res := ProxyCheck{Ms: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = friendlyNetErr(err)
		return res
	}
	res.OK, res.IP, res.Country, res.City = true, info.IP, info.Country, info.City
	return res
}

func getJSON(c *http.Client, u string, v any) error {
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(v)
}

func friendlyNetErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "407"), strings.Contains(s, "username/password authentication failed"):
		return "Sai tài khoản/mật khẩu proxy"
	case strings.Contains(s, "connection refused"), strings.Contains(s, "actively refused"):
		return "Không kết nối được (sai địa chỉ/cổng, hoặc máy đích đang tắt/chặn)"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline exceeded"):
		return "Hết thời gian chờ (mạng chậm hoặc máy đích không phản hồi)"
	case strings.Contains(s, "no such host"):
		return "Không tìm thấy địa chỉ (tên miền sai hoặc chưa trỏ DNS)"
	}
	return s
}

// ParseProxyLine accepts:
//
//	host:port
//	host:port:user:pass
//	user:pass@host:port
//	http://user:pass@host:port  /  socks5://...
func ParseProxyLine(line, defType string) (Proxy, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Proxy{}, errors.New("dòng trống")
	}
	if defType == "" {
		defType = "http"
	}
	if i := strings.Index(line, "://"); i > 0 {
		u, err := url.Parse(line)
		if err != nil {
			return Proxy{}, err
		}
		t := strings.ToLower(u.Scheme)
		switch t {
		case "socks5", "socks5h":
			t = "socks5"
		case "http", "https":
			t = "http"
		default:
			return Proxy{}, fmt.Errorf("loại proxy %q không hỗ trợ", u.Scheme)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || u.Hostname() == "" {
			return Proxy{}, errors.New("thiếu host hoặc cổng")
		}
		p := Proxy{Type: t, Host: u.Hostname(), Port: port}
		if u.User != nil {
			p.User = u.User.Username()
			p.Pass, _ = u.User.Password()
		}
		return p, nil
	}
	p := Proxy{Type: defType}
	hostPart := line
	if at := strings.LastIndex(line, "@"); at > 0 {
		cred := line[:at]
		hostPart = line[at+1:]
		user, pass, _ := strings.Cut(cred, ":")
		p.User, p.Pass = user, pass
	}
	parts := strings.Split(hostPart, ":")
	if len(parts) != 2 && len(parts) != 4 {
		return Proxy{}, errors.New("sai định dạng (cần host:port hoặc host:port:user:pass)")
	}
	port, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || port <= 0 || port > 65535 {
		return Proxy{}, errors.New("cổng không hợp lệ")
	}
	p.Host, p.Port = strings.TrimSpace(parts[0]), port
	if len(parts) == 4 {
		p.User, p.Pass = parts[2], parts[3]
	}
	if p.Host == "" {
		return Proxy{}, errors.New("thiếu host")
	}
	return p, nil
}
