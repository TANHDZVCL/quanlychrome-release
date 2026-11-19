package main

// "Trung tâm Zalo": the Windows app on the computer that holds the Zalo
// profiles (the agent) streams screenshots of those Chrome tabs here, and a
// browser anywhere (the viewer, /hub) sees them and sends clicks/keys back.
// Transport is Server-Sent Events + POST so it works behind the existing
// nginx config (no WebSocket upgrade needed).

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed hub
var hubFS embed.FS

const hubCookie = "qlc_hub"

type hubSession struct {
	user    string
	expires time.Time
}

type hubRoom struct {
	mu        sync.Mutex
	agent     chan []byte // commands to the agent
	agentGen  int
	viewers   map[int]chan []byte
	nextID    int
	panels    []byte            // last "panels" message
	frames    map[string][]byte // last frame per panel
	agentName string
}

type hubState struct {
	mu       sync.Mutex
	rooms    map[string]*hubRoom
	sessions map[string]*hubSession // key = hex sha256(token)
}

var hubs = &hubState{rooms: map[string]*hubRoom{}, sessions: map[string]*hubSession{}}

func (h *hubState) room(u string) *hubRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[u]
	if r == nil {
		r = &hubRoom{viewers: map[int]chan []byte{}, frames: map[string][]byte{}}
		h.rooms[u] = r
	}
	return r
}

func (r *hubRoom) toAgentLocked(msg []byte) {
	if r.agent == nil {
		return
	}
	select {
	case r.agent <- msg:
	default: // agent too slow: drop
	}
}

func (r *hubRoom) toViewersLocked(msg []byte) {
	for _, ch := range r.viewers {
		select {
		case ch <- msg:
		default:
		}
	}
}

func jsonMsg(v any) []byte { b, _ := json.Marshal(v); return b }

func (s *server) hubRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(hubFS, "hub")
	files := http.FileServer(http.FS(sub))
	mux.Handle("GET /hub/", http.StripPrefix("/hub/", noCache(files)))
	mux.HandleFunc("GET /hub", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/hub/", http.StatusFound) })
	mux.HandleFunc("POST /hub/login", s.hubLogin)
	mux.HandleFunc("POST /hub/logout", s.hubLogout)
	mux.HandleFunc("GET /hub/me", s.hubViewer(s.hubMe))
	mux.HandleFunc("GET /hub/stream", s.hubViewer(s.hubStream))
	mux.HandleFunc("POST /hub/input", s.hubViewer(s.hubInput))
	mux.HandleFunc("POST /api/hub/password", s.auth(s.hubSetPassword))
	mux.HandleFunc("GET /api/hub/agent", s.auth(s.hubAgent))
	mux.HandleFunc("POST /api/hub/agent/push", s.auth(s.hubPush))
}

// ---------- viewer auth ----------

func (s *server) hubSetPassword(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || len(in.Password) < 8 || len(in.Password) > 72 {
		fail(w, 400, "Mật khẩu Trung tâm Zalo cần 8–72 ký tự")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.Password), 11)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.mu.Lock()
	s.users[u].HubHash = string(h)
	err = s.saveUsersLocked()
	s.mu.Unlock()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// log out every viewer of this user
	hubs.mu.Lock()
	for k, v := range hubs.sessions {
		if v.user == u {
			delete(hubs.sessions, k)
		}
	}
	hubs.mu.Unlock()
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) hubLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	ip := clientIP(r)
	if s.tooManyFails("hubip:"+ip, "hubuser:"+in.Username) {
		fail(w, 429, "Sai quá nhiều lần, hãy đợi 10 phút")
		return
	}
	s.mu.Lock()
	usr := s.users[in.Username]
	hash := ""
	if usr != nil {
		hash = usr.HubHash
	}
	s.mu.Unlock()
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		s.recordFail("hubip:"+ip, "hubuser:"+in.Username)
		fail(w, 401, "Sai tên đăng nhập hoặc mật khẩu Trung tâm Zalo")
		return
	}
	tok := newToken()
	hubs.mu.Lock()
	hubs.sessions[hashHex([]byte(tok))] = &hubSession{user: in.Username, expires: time.Now().Add(30 * 24 * time.Hour)}
	hubs.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: hubCookie, Value: tok, Path: "/hub", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
	log.Printf("hub login %s from %s", in.Username, ip)
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) hubLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(hubCookie); err == nil {
		hubs.mu.Lock()
		delete(hubs.sessions, hashHex([]byte(c.Value)))
		hubs.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: hubCookie, Value: "", Path: "/hub", MaxAge: -1, HttpOnly: true, Secure: true})
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) hubViewer(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(hubCookie)
		var u string
		if err == nil {
			hubs.mu.Lock()
			if sess := hubs.sessions[hashHex([]byte(c.Value))]; sess != nil && time.Now().Before(sess.expires) {
				u = sess.user
			}
			hubs.mu.Unlock()
		}
		if u == "" {
			fail(w, 401, "Chưa đăng nhập")
			return
		}
		h(w, r, u)
	}
}

func (s *server) hubMe(w http.ResponseWriter, r *http.Request, u string) {
	room := hubs.room(u)
	room.mu.Lock()
	online, name := room.agent != nil, room.agentName
	room.mu.Unlock()
	reply(w, 200, map[string]any{"user": u, "online": online, "agentName": name})
}

// ---------- streams ----------

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer
	w.WriteHeader(200)
}

// pump writes messages from ch as SSE until the client goes away.
func pump(w http.ResponseWriter, r *http.Request, ch <-chan []byte) {
	fl, _ := w.(http.Flusher)
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(append(append([]byte("data: "), msg...), '\n', '\n')); err != nil {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

func (s *server) hubStream(w http.ResponseWriter, r *http.Request, u string) {
	room := hubs.room(u)
	ch := make(chan []byte, 64)
	room.mu.Lock()
	id := room.nextID
	room.nextID++
	room.viewers[id] = ch
	ch <- jsonMsg(map[string]any{"t": "agent", "online": room.agent != nil, "name": room.agentName})
	if room.panels != nil {
		ch <- room.panels
	}
	for _, f := range room.frames {
		select {
		case ch <- f:
		default:
		}
	}
	room.toAgentLocked(jsonMsg(map[string]any{"t": "viewers", "n": len(room.viewers)}))
	room.mu.Unlock()
	defer func() {
		room.mu.Lock()
		delete(room.viewers, id)
		room.toAgentLocked(jsonMsg(map[string]any{"t": "viewers", "n": len(room.viewers)}))
		room.mu.Unlock()
	}()
	sseHeaders(w)
	pump(w, r, ch)
}

func (s *server) hubInput(w http.ResponseWriter, r *http.Request, u string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || !json.Valid(body) {
		fail(w, 400, "dữ liệu không hợp lệ")
		return
	}
	// A batch (JSON array) keeps the order of quick inputs such as typing.
	var msgs [][]byte
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(t, &arr) != nil || len(arr) > 500 {
			fail(w, 400, "dữ liệu không hợp lệ")
			return
		}
		for _, m := range arr {
			var c bytes.Buffer
			if json.Compact(&c, m) == nil {
				msgs = append(msgs, c.Bytes())
			}
		}
	} else {
		var c bytes.Buffer
		_ = json.Compact(&c, body)
		msgs = [][]byte{c.Bytes()}
	}
	room := hubs.room(u)
	room.mu.Lock()
	online := room.agent != nil
	for _, m := range msgs {
		room.toAgentLocked(m)
	}
	room.mu.Unlock()
	if !online {
		fail(w, 503, "Máy chứa Zalo đang không kết nối")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *server) hubAgent(w http.ResponseWriter, r *http.Request, u string, sess *session) {
	room := hubs.room(u)
	ch := make(chan []byte, 256)
	room.mu.Lock()
	if room.agent != nil {
		close(room.agent) // a newer agent replaces the old one
	}
	room.agent = ch
	room.agentGen++
	gen := room.agentGen
	room.agentName = sess.Device
	ch <- jsonMsg(map[string]any{"t": "viewers", "n": len(room.viewers)})
	room.toViewersLocked(jsonMsg(map[string]any{"t": "agent", "online": true, "name": sess.Device}))
	room.mu.Unlock()
	log.Printf("hub agent connected: %s (%s)", u, sess.Device)
	defer func() {
		room.mu.Lock()
		if room.agentGen == gen {
			room.agent = nil
			room.frames = map[string][]byte{}
			room.toViewersLocked(jsonMsg(map[string]any{"t": "agent", "online": false}))
		}
		room.mu.Unlock()
	}()
	sseHeaders(w)
	pump(w, r, ch)
}

// hubPush receives newline-separated JSON messages from the agent.
func (s *server) hubPush(w http.ResponseWriter, r *http.Request, u string, _ *session) {
	room := hubs.room(u)
	sc := bufio.NewScanner(io.LimitReader(r.Body, 32<<20))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		msg := append([]byte(nil), line...)
		var head struct {
			T string `json:"t"`
			P string `json:"p"`
		}
		_ = json.Unmarshal(msg, &head)
		room.mu.Lock()
		switch head.T {
		case "panels":
			room.panels = msg
		case "frame":
			room.frames[head.P] = msg
		}
		room.toViewersLocked(msg)
		room.mu.Unlock()
		n++
	}
	reply(w, 200, map[string]int{"n": n})
}
