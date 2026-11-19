package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// fake upstream HTTP proxy requiring user:pass
func startHTTPUpstream(t *testing.T, user, pass string) (string, *int) {
	hits := new(int)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if r.Header.Get("Proxy-Authorization") != want {
			w.Header().Set("Proxy-Authenticate", "Basic")
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			up, err := net.Dial("tcp", r.Host)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			c, _, _ := w.(http.Hijacker).Hijack()
			io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
			go pipe(c, up)
			return
		}
		r.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("X-Via", "upstream")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), hits
}

// minimal SOCKS5 server with username/password auth
func startSocksUpstream(t *testing.T, user, pass string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				h := make([]byte, 2)
				io.ReadFull(br, h)
				io.ReadFull(br, make([]byte, h[1]))
				c.Write([]byte{5, 2})
				// RFC1929
				io.ReadFull(br, h[:1])
				ul, _ := br.ReadByte()
				u := make([]byte, ul)
				io.ReadFull(br, u)
				pl, _ := br.ReadByte()
				p := make([]byte, pl)
				io.ReadFull(br, p)
				if string(u) != user || string(p) != pass {
					c.Write([]byte{1, 1})
					c.Close()
					return
				}
				c.Write([]byte{1, 0})
				req := make([]byte, 4)
				io.ReadFull(br, req)
				var host string
				switch req[3] {
				case 1:
					b := make([]byte, 4)
					io.ReadFull(br, b)
					host = net.IP(b).String()
				case 3:
					l, _ := br.ReadByte()
					b := make([]byte, l)
					io.ReadFull(br, b)
					host = string(b)
				}
				pb := make([]byte, 2)
				io.ReadFull(br, pb)
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))))
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					c.Close()
					return
				}
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(&bufConn{Conn: c, r: br}, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func clientVia(fwd *Forwarder) *http.Client {
	u, _ := url.Parse("http://" + fwd.Addr())
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

func targets(t *testing.T) (plain, secure *httptest.Server) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello") })
	plain, secure = httptest.NewServer(h), httptest.NewTLSServer(h)
	t.Cleanup(plain.Close)
	t.Cleanup(secure.Close)
	return
}

func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func splitHP(t *testing.T, a string) (string, int) {
	h, p, _ := net.SplitHostPort(a)
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestForwarderHTTPUpstream(t *testing.T) {
	plain, secure := targets(t)
	addr, hits := startHTTPUpstream(t, "u1", "p:w@d")
	h, p := splitHP(t, addr)
	fwd, err := StartForwarder(Proxy{Type: "http", Host: h, Port: p, User: "u1", Pass: "p:w@d"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	c := clientVia(fwd)
	if got := get(t, c, plain.URL); got != "hello" {
		t.Fatalf("plain: %q", got)
	}
	if got := get(t, c, secure.URL); got != "hello" {
		t.Fatalf("tls: %q", got)
	}
	if *hits < 2 {
		t.Fatalf("upstream not used: %d", *hits)
	}
}

func TestForwarderHTTPUpstreamWrongPass(t *testing.T) {
	_, secure := targets(t)
	addr, _ := startHTTPUpstream(t, "u1", "right")
	h, p := splitHP(t, addr)
	fwd, _ := StartForwarder(Proxy{Type: "http", Host: h, Port: p, User: "u1", Pass: "wrong"}, 0)
	defer fwd.Close()
	if _, err := clientVia(fwd).Get(secure.URL); err == nil {
		t.Fatal("expected error with wrong password")
	}
}

func TestForwarderSocksUpstream(t *testing.T) {
	plain, secure := targets(t)
	addr := startSocksUpstream(t, "su", "sp")
	h, p := splitHP(t, addr)
	fwd, err := StartForwarder(Proxy{Type: "socks5", Host: h, Port: p, User: "su", Pass: "sp"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	c := clientVia(fwd)
	if got := get(t, c, plain.URL); got != "hello" {
		t.Fatalf("plain: %q", got)
	}
	if got := get(t, c, secure.URL); got != "hello" {
		t.Fatalf("tls: %q", got)
	}
}

func TestForwarderFixedPort(t *testing.T) {
	f1, _ := StartForwarder(Proxy{Type: "http", Host: "127.0.0.1", Port: 1}, 0)
	port := f1.Port()
	f1.Close()
	f2, err := StartForwarder(Proxy{Type: "http", Host: "127.0.0.1", Port: 1}, port)
	if err != nil || f2.Port() != port {
		t.Fatalf("want port %d got %v %v", port, f2, err)
	}
	f2.Close()
}

func TestParseProxyLine(t *testing.T) {
	cases := []struct {
		in   string
		want Proxy
	}{
		{"1.2.3.4:8080", Proxy{Type: "http", Host: "1.2.3.4", Port: 8080}},
		{"1.2.3.4:8080:user:pass", Proxy{Type: "http", Host: "1.2.3.4", Port: 8080, User: "user", Pass: "pass"}},
		{"user:pass@1.2.3.4:8080", Proxy{Type: "http", Host: "1.2.3.4", Port: 8080, User: "user", Pass: "pass"}},
		{"socks5://u:p@host.com:1080", Proxy{Type: "socks5", Host: "host.com", Port: 1080, User: "u", Pass: "p"}},
		{"http://h:3128", Proxy{Type: "http", Host: "h", Port: 3128}},
	}
	for _, c := range cases {
		got, err := ParseProxyLine(c.in, "http")
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"abc", "1.2.3.4", "1.2.3.4:99999", "1.2.3.4:80:u", "ftp://h:1"} {
		if _, err := ParseProxyLine(bad, "http"); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}
