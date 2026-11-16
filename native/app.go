package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type SavedHost struct {
	Origin string `json:"origin"`
	Pin    string `json:"pin"`
}
type PendingHost struct {
	Origin, Pin string
	Expires     time.Time
}
type WebSession struct {
	mu               sync.Mutex
	ID, CSRF, Notice string
	Local            bool
	Expires          time.Time
	Remote           *Remote
	Pending          *PendingHost
	Sent             map[string]bool
	Authed           atomic.Bool
}
type App struct {
	discoveryMu  sync.Mutex
	discovery    *Discovery
	Store        *Store
	API          *API
	templates    *template.Template
	sessionsMu   sync.Mutex
	sessions     map[string]*WebSession
	hostMu       sync.Mutex
	host         *http.Server
	ListenerAddr string
	localPort    string
	launchMu     sync.Mutex
	savedMu      sync.Mutex
	saved        []SavedHost
	uiUploads    chan struct{}
	webRate      Limiter
	reader       *ReaderBridge
	shutdown     chan struct{}
	shutdownOnce sync.Once
}
type Page struct {
	VaultPending                                                                            bool
	VaultEnabled, VaultLocked                                                               bool
	AdminTab                                                                                string
	Title, View, CSRF, Notice, Error, Address, Code, Pin, JoinCode, Username, Query, SendID string
	Local, Hosting, Setup, Admin, Authenticated, More, Older                                bool
	Session                                                                                 SessionInfo
	Messages                                                                                []Message
	Files                                                                                   []FileInfo
	Users                                                                                   []User
	Pending                                                                                 []User
	Saved                                                                                   []SavedHost
	Addresses                                                                               []string
	Before                                                                                  int64
	UploadLimit                                                                             int64
	Counts                                                                                  map[string]int64
}

func newApp(s *Store) (*App, error) {
	templates, e := template.New("pages").Funcs(template.FuncMap{"size": func(n int64) string {
		if n < 1024 {
			return fmt.Sprintf("%d B", n)
		}
		if n < 1<<20 {
			return fmt.Sprintf("%.0f KB", float64(n)/1024)
		}
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}, "shortTime": func(v string) string {
		t, e := time.Parse(time.RFC3339, v)
		if e != nil {
			return v
		}
		return t.Local().Format("02 Jan 15:04")
	}}).ParseFS(assets, "ui/*.html")
	if e != nil {
		return nil, e
	}
	a := &App{Store: s, API: newAPI(s), templates: templates, sessions: map[string]*WebSession{}, localPort: "8001", uiUploads: make(chan struct{}, 2)}
	b, _ := os.ReadFile(filepath.Join(s.Data, "trusted-hosts.json"))
	_ = json.Unmarshal(b, &a.saved)
	if len(a.saved) > 8 {
		a.saved = a.saved[:8]
	}
	return a, nil
}
func serverFor(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
}
func (a *App) hosting() bool { a.hostMu.Lock(); defer a.hostMu.Unlock(); return a.host != nil }
func (a *App) startHost() error {
	a.Store.Vault.mu.RLock()
	ready := a.Store.Vault.ready()
	a.Store.Vault.mu.RUnlock()
	if !ready {
		return errors.New("Unlock saved-data encryption in Home → Local privacy first.")
	}
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if a.host != nil {
		return nil
	}
	listener, e := net.Listen("tcp", ":8000")
	if e != nil {
		return errors.New("Port 8000 is busy. Stop the previous chat server, then try again.")
	}
	server := serverFor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := net.ParseIP(clientIP(r))
		if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
			http.Error(w, "This server accepts private-network clients only.", 403)
			return
		}
		if a.API.ipBanned(ip.String()) {
			http.Error(w, "This IP address is banned by the host.", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			a.API.ServeHTTP(w, r)
		} else {
			a.web(w, r, false)
		}
	}))
	server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{a.Store.Cert}}
	a.host = server
	a.ListenerAddr = "https://127.0.0.1:8000"
	go func() {
		err := server.Serve(tls.NewListener(limitConnections(listener, 24), server.TLSConfig))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print("Host listener stopped: ", err)
		}
		a.hostMu.Lock()
		if a.host == server {
			a.closeDiscovery()
			a.host = nil
		}
		a.hostMu.Unlock()
	}()
	return nil
}
func (a *App) stopHost() {
	a.hostMu.Lock()
	server := a.host
	a.closeDiscovery()
	a.host = nil
	a.hostMu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
	}
}
func (a *App) ownOrigin() string {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if a.ListenerAddr != "" {
		return a.ListenerAddr
	}
	return "https://127.0.0.1:8000"
}
func (a *App) ownRemote() *Remote { return newLocalRemote(a.ownOrigin(), a.Store.Fingerprint, a.API) }
func (a *App) getSession(w http.ResponseWriter, r *http.Request, local bool) (*WebSession, bool) {
	name := "nc_web"
	if local {
		name = "nc_local"
	}
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	now := time.Now()
	if c, e := r.Cookie(name); e == nil {
		if s, ok := a.sessions[c.Value]; ok && s.Local == local && now.Before(s.Expires) {
			return s, true
		}
	}
	for k, s := range a.sessions {
		if now.After(s.Expires) {
			delete(a.sessions, k)
		}
	}
	if local {
		http.Error(w, "Open Neighbour Chat from your Kindle library.", 403)
		return nil, false
	}
	if !a.webRate.Allow("guest:"+clientIP(r), 60, time.Minute) {
		http.Error(w, "Too many new sessions. Try again shortly.", 429)
		return nil, false
	}
	if len(a.sessions) >= 128 {
		for key, value := range a.sessions {
			if !value.Local && !value.Authed.Load() {
				delete(a.sessions, key)
				break
			}
		}
	}
	if len(a.sessions) >= 128 {
		http.Error(w, "Too many open sessions. Try again later.", 503)
		return nil, false
	}
	s := &WebSession{ID: randomToken(32), CSRF: randomToken(32), Expires: now.Add(12 * time.Hour), Remote: a.ownRemote(), Sent: map[string]bool{}}
	a.sessions[s.ID] = s
	http.SetCookie(w, &http.Cookie{Name: name, Value: s.ID, Path: "/", Secure: !local, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	return s, true
}
func (a *App) rotateWeb(w http.ResponseWriter, old *WebSession, authenticated bool) {
	next := &WebSession{ID: randomToken(32), CSRF: randomToken(32), Notice: old.Notice, Local: old.Local, Expires: time.Now().Add(12 * time.Hour), Remote: old.Remote, Sent: map[string]bool{}}
	next.Authed.Store(authenticated)
	a.sessionsMu.Lock()
	delete(a.sessions, old.ID)
	a.sessions[next.ID] = next
	a.sessionsMu.Unlock()
	old.Remote = nil
	old.CSRF = randomToken(32)
	name := "nc_web"
	if next.Local {
		name = "nc_local"
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: next.ID, Path: "/", Secure: !next.Local, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
}
func (a *App) launch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || !localRequest(r) {
		http.Error(w, "Open this app from the Kindle library.", 403)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if e := r.ParseForm(); e != nil {
		http.Error(w, "Invalid launch request.", 400)
		return
	}
	a.launchMu.Lock()
	defer a.launchMu.Unlock()
	var ticket LaunchTicket
	b, _ := os.ReadFile(filepath.Join(a.Store.Data, "launch-ticket.json"))
	_ = json.Unmarshal(b, &ticket)
	if ticket.Token == "" || time.Now().Unix() > ticket.Expires || !equal(ticket.Token, r.FormValue("token")) {
		http.Error(w, "This launch has expired. Reopen Neighbour Chat from the library.", 403)
		return
	}
	if e := os.Remove(filepath.Join(a.Store.Data, "launch-ticket.json")); e != nil {
		http.Error(w, "Could not finish opening the app. Reopen it.", 500)
		return
	}
	s := &WebSession{ID: randomToken(32), CSRF: randomToken(32), Local: true, Expires: time.Now().Add(12 * time.Hour), Sent: map[string]bool{}}
	a.sessionsMu.Lock()
	for key, value := range a.sessions {
		if value.Local {
			delete(a.sessions, key)
		}
	}
	if c, e := r.Cookie("nc_local"); e == nil {
		delete(a.sessions, c.Value)
	}
	if len(a.sessions) >= 128 {
		for k, v := range a.sessions {
			if time.Now().After(v.Expires) {
				delete(a.sessions, k)
			}
		}
	}
	if len(a.sessions) >= 128 {
		a.sessionsMu.Unlock()
		http.Error(w, "Too many open sessions. Restart the app.", 503)
		return
	}
	a.sessions[s.ID] = s
	a.sessionsMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "nc_local", Value: s.ID, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	if ticket.Next == "/uninstall" {
		http.Redirect(w, r, "/uninstall", 303)
	} else {
		http.Redirect(w, r, "/home", 303)
	}
}
func (a *App) remember(origin, pin string) error {
	a.savedMu.Lock()
	defer a.savedMu.Unlock()
	hosts := []SavedHost{{origin, pin}}
	for _, h := range a.saved {
		if h.Origin != origin && len(hosts) < 8 {
			hosts = append(hosts, h)
		}
	}
	b, e := json.Marshal(hosts)
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(a.Store.Data, "trusted-hosts.json"), b, 0600); e != nil {
		return e
	}
	a.saved = hosts
	return nil
}
func (a *App) basePage(s *WebSession, view string) Page {
	p := Page{Title: "Neighbour Chat", View: view, CSRF: s.CSRF, Local: s.Local, Hosting: a.hosting(), Code: checkCode(a.Store.Fingerprint), Pin: a.Store.Fingerprint, JoinCode: a.Store.joinCode(), Setup: readText(filepath.Join(a.Store.Data, "initial-admin-password.txt")) != "", Notice: s.Notice, UploadLimit: 25}
	s.Notice = ""
	p.VaultEnabled, p.VaultLocked = a.Store.Vault.status()
	p.VaultPending = a.Store.Vault.pending()
	a.savedMu.Lock()
	p.Saved = append([]SavedHost(nil), a.saved...)
	a.savedMu.Unlock()
	p.Addresses = addresses()
	if s.Remote != nil {
		p.Address = s.Remote.Origin
	}
	return p
}
func (a *App) render(w http.ResponseWriter, p Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if e := a.templates.ExecuteTemplate(w, "layout", p); e != nil {
		log.Print("Could not render app page")
	}
}
func (a *App) web(w http.ResponseWriter, r *http.Request, local bool) {
	securityHeaders(w)
	if local {
		host, port, e := net.SplitHostPort(r.Host)
		if e != nil || !localRequest(r) || (host != "127.0.0.1" && host != "localhost") || port != a.localPort {
			http.Error(w, "This controller is only available on this Kindle.", 403)
			return
		}
		if r.URL.Path == "/_health" && r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"service": "neighbour-chat-v3", "ok": true, "reader_api": 4})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_reader/") {
			if a.reader == nil {
				http.Error(w, "Reader service is not running.", 503)
				return
			}
			a.reader.serve(w, r)
			return
		}
		if r.URL.Path == "/_launch" {
			a.launch(w, r)
			return
		}
	}
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	// Public immutable content has no session state. Revalidate by content hash
	// so old WebKit can reuse bytes without retaining sensitive HTML pages.
	if r.URL.Path == "/app.css" || r.URL.Path == "/app.js" {
		if r.Method != "GET" {
			http.Error(w, "Use GET for app resources.", http.StatusMethodNotAllowed)
			return
		}
		file, contentType := "ui/app.css", "text/css; charset=utf-8"
		if r.URL.Path == "/app.js" {
			file, contentType = "ui/app.js", "application/javascript; charset=utf-8"
		}
		b, _ := assets.ReadFile(file)
		etag := fmt.Sprintf("\"%x\"", sha256.Sum256(b))
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(b)
		return
	}
	s, ok := a.getSession(w, r, local)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == "POST" {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			scheme := "https"
			if local {
				scheme = "http"
			}
			if origin != scheme+"://"+r.Host {
				http.Error(w, "Invalid request origin.", 403)
				return
			}
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if r.URL.Path != "/upload" {
				http.Error(w, "Invalid upload action.", 400)
				return
			}
			a.streamWebUpload(w, r, s)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if e := r.ParseForm(); e != nil {
			http.Error(w, "Invalid or oversized form.", 400)
			return
		}
		if !equal(r.FormValue("csrf"), s.CSRF) {
			http.Error(w, "Your form expired. Refresh this page before trying again.", 400)
			return
		}
	}
	pth := r.URL.Path
	if pth == "/" {
		if local {
			http.Redirect(w, r, "/home", 303)
		} else {
			http.Redirect(w, r, "/chat", 303)
		}
		return
	}
	if pth == "/kindle" {
		http.Redirect(w, r, "/chat", 303)
		return
	}
	if local && (pth == "/home" || pth == "/host" || pth == "/join" || pth == "/confirm" || pth == "/reconnect" || pth == "/details" || pth == "/setup" || pth == "/uninstall" || pth == "/privacy") {
		a.controls(w, r, s)
		return
	}
	if !local && (pth == "/home" || pth == "/host" || pth == "/join" || pth == "/setup" || pth == "/details") {
		http.NotFound(w, r)
		return
	}
	if s.Remote == nil {
		http.Redirect(w, r, "/home", 303)
		return
	}
	info, e := s.Remote.info()
	if e != nil {
		p := a.basePage(s, "offline")
		p.Error = e.Error()
		a.render(w, p)
		return
	}
	s.Authed.Store(info.Authenticated)
	if pth == "/login" || pth == "/register" {
		a.authPage(w, r, s, info)
		return
	}
	if !info.Authenticated {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if r.Method == "POST" {
		a.chatAction(w, r, s, info)
		return
	}
	p := a.basePage(s, "chat")
	p.Session = info
	p.Authenticated = true
	p.Admin = info.Admin
	switch {
	case pth == "/chat" || pth == "/files" || pth == "/people":
		var data ChatData
		query := "/api/chat-data?section=" + strings.TrimPrefix(pth, "/") + "&page_size=12"
		if b, e := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); e == nil && b > 0 {
			query += "&before=" + strconv.FormatInt(b, 10)
			p.Older = true
		}
		if e = s.Remote.call("GET", query, nil, &data); e != nil {
			p.Error = e.Error()
		} else {
			p.Messages = data.Messages
			p.Files = data.Files
			p.Users = data.Users
			p.More = data.More
			if len(data.Messages) > 0 {
				p.Before = data.Messages[0].ID
			}
			// Keep the API's chronological contract; present newest first on e-ink.
			for i, j := 0, len(p.Messages)-1; i < j; i, j = i+1, j-1 {
				p.Messages[i], p.Messages[j] = p.Messages[j], p.Messages[i]
			}
			if data.UploadLimit > 0 {
				p.UploadLimit = data.UploadLimit
			}
		}
		if pth == "/files" {
			p.View = "files"
		}
		if pth == "/people" {
			p.View = "people"
		}
	case pth == "/search":
		p.View = "search"
		p.Query = r.URL.Query().Get("q")
		if p.Query != "" {
			var data struct {
				Results []Message `json:"results"`
			}
			e = s.Remote.call("GET", "/api/search?q="+urlQuery(p.Query), nil, &data)
			if e != nil {
				p.Error = e.Error()
			}
			p.Messages = data.Results
		}
	case pth == "/admin" && info.Admin:
		p.View = "admin"
		p.AdminTab = adminTab(r.URL.Query().Get("tab"))
		var data AdminData
		e = s.Remote.call("GET", "/api/admin", nil, &data)
		if e != nil {
			p.Error = e.Error()
		}
		p.Users = data.Users
		p.Pending = data.Pending
		p.Files = data.Files
		p.Messages = data.Messages
		p.Counts = data.Counts
		p.UploadLimit = data.UploadLimit
	case strings.HasPrefix(pth, "/download/"):
		a.fileDownload(w, r, s)
		return
	default:
		http.NotFound(w, r)
		return
	}
	p.SendID = randomToken(24)
	a.render(w, p)
}
func urlQuery(v string) string { return url.QueryEscape(v) }
func (a *App) controls(w http.ResponseWriter, r *http.Request, s *WebSession) {
	p := a.basePage(s, "home")
	switch r.URL.Path {
	case "/privacy":
		p.View = "privacy"
		if r.Method == "POST" {
			action := r.FormValue("action")
			a.stopHost()
			var err error
			switch action {
			case "enable":
				if r.FormValue("password") != r.FormValue("confirm") {
					err = errors.New("The passwords do not match.")
				} else {
					err = a.Store.enableEncryption(r.FormValue("password"))
				}
			case "unlock":
				err = a.Store.unlockEncryption(r.FormValue("password"))
			case "disable":
				err = a.Store.disableEncryption(r.FormValue("password"))
			case "lock":
				a.Store.Vault.lock()
			default:
				err = errors.New("Invalid privacy action.")
			}
			if err != nil {
				p.Error = err.Error()
			} else {
				s.Notice = "Saved-data privacy settings updated. Wi-Fi transfers always remain encrypted."
				http.Redirect(w, r, "/privacy", 303)
				return
			}
			p.VaultEnabled, p.VaultLocked = a.Store.Vault.status()
			p.VaultPending = a.Store.Vault.pending()
		}
	case "/uninstall":
		p.View = "uninstall"
		if r.Method == "POST" {
			if r.FormValue("confirm") != "yes" {
				p.Error = "Confirm that you want to stop and unregister the app."
				break
			}
			if e := a.scheduleUninstall(); e != nil {
				p.Error = "Could not start uninstalling. Check NeighbourChat.log in Internal Storage and try again."
				break
			}
			p.View = "uninstalling"
		}
	case "/home":
		if r.Method != "GET" {
			http.Error(w, "Use the app controls.", 405)
			return
		}
	case "/details":
		p.View = "details"
	case "/host":
		if r.Method != "POST" {
			http.Error(w, "Use the hosting button.", 405)
			return
		}
		action := r.FormValue("action")
		if action == "stop" {
			a.stopHost()
			s.Notice = "Hosting stopped. Your messages and files are saved."
			http.Redirect(w, r, "/home", 303)
			return
		}
		if action != "start" && action != "open" {
			http.Error(w, "Invalid hosting action.", 400)
			return
		}
		if p.VaultLocked {
			http.Redirect(w, r, "/privacy", 303)
			return
		}
		if p.Setup {
			http.Redirect(w, r, "/setup", 303)
			return
		}
		if e := a.startHost(); e != nil {
			p.Error = e.Error()
			break
		}
		if s.Remote == nil || s.Remote.Origin != a.ownOrigin() || s.Remote.Pin != a.Store.Fingerprint {
			s.Remote = a.ownRemote()
		}
		http.Redirect(w, r, "/chat", 303)
		return
	case "/setup":
		p.View = "setup"
		if !p.Setup {
			http.Redirect(w, r, "/home", 303)
			return
		}
		if r.Method == "POST" {
			password := r.FormValue("password")
			if password != r.FormValue("confirm") {
				p.Error = "The passwords do not match."
				break
			}
			initial := readText(filepath.Join(a.Store.Data, "initial-admin-password.txt"))
			if e := a.Store.changeAdmin(initial, password); e != nil {
				p.Error = "Could not set that password. Use 10 to 256 characters."
				break
			}
			if e := a.startHost(); e != nil {
				p.Error = e.Error()
				break
			}
			s.Remote = a.ownRemote()
			if _, e := s.Remote.info(); e != nil {
				p.Error = e.Error()
				break
			}
			if e := s.Remote.call("POST", "/api/login", map[string]string{"username": "admin", "password": password}, nil); e != nil {
				p.Error = e.Error()
				break
			}
			s.Notice = "Hosting is ready. Share the address and join code from Home."
			a.rotateWeb(w, s, true)
			http.Redirect(w, r, "/chat", 303)
			return
		}
	case "/join":
		p.View = "join"
		if r.Method == "POST" {
			origin, e := privateOrigin(r.FormValue("address"))
			if e != nil {
				p.Error = e.Error()
				break
			}
			pin, e := probeCertificate(origin)
			if e != nil {
				p.Error = e.Error()
				break
			}
			s.Pending = &PendingHost{Origin: origin, Pin: pin, Expires: time.Now().Add(3 * time.Minute)}
			p.View = "confirm"
			p.Address = origin
			p.Pin = pin
			p.Code = checkCode(pin)
		}
	case "/confirm":
		if r.Method != "POST" || s.Pending == nil || time.Now().After(s.Pending.Expires) {
			p.Error = "The connection check expired. Enter the host address again."
			p.View = "join"
			break
		}
		if r.FormValue("confirm") != "yes" {
			s.Pending = nil
			http.Redirect(w, r, "/home", 303)
			return
		}
		pending := s.Pending
		s.Pending = nil
		s.Remote = newRemote(pending.Origin, pending.Pin)
		if _, e := s.Remote.info(); e != nil {
			p.View = "join"
			p.Error = e.Error()
			break
		}
		if e := a.remember(pending.Origin, pending.Pin); e != nil {
			p.View = "join"
			p.Error = "Connected, but could not save this host. Check free storage."
			break
		}
		http.Redirect(w, r, "/chat", 303)
		return
	case "/reconnect":
		if r.Method != "POST" {
			http.Error(w, "Use a saved host button.", 405)
			return
		}
		index, e := strconv.Atoi(r.FormValue("host"))
		a.savedMu.Lock()
		var saved SavedHost
		if e == nil && index >= 0 && index < len(a.saved) {
			saved = a.saved[index]
		}
		a.savedMu.Unlock()
		if saved.Origin == "" {
			http.Error(w, "Saved host not found.", 400)
			return
		}
		origin, e := privateOrigin(saved.Origin)
		if e != nil {
			p.Error = "That saved address is invalid. Connect again."
			break
		}
		s.Remote = newRemote(origin, saved.Pin)
		if _, e := s.Remote.info(); e != nil {
			p.Error = e.Error()
			break
		}
		http.Redirect(w, r, "/chat", 303)
		return
	}
	a.render(w, p)
}
func (a *App) scheduleUninstall() error {
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	logfile, e := os.OpenFile(diagnosticLogPath(a.Store.Root), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer logfile.Close()
	command := exec.Command(executable, "uninstall", "--root", a.Store.Root, "--delay", "2", "--remove-files")
	command.Stdout = logfile
	command.Stderr = logfile
	if e = command.Start(); e != nil {
		return e
	}
	go func() { _ = command.Wait() }()
	return nil
}
func (a *App) authPage(w http.ResponseWriter, r *http.Request, s *WebSession, info SessionInfo) {
	view := "login"
	if r.URL.Path == "/register" {
		view = "register"
	}
	if info.Authenticated {
		http.Redirect(w, r, "/chat", 303)
		return
	}
	p := a.basePage(s, view)
	if s.Remote.Origin == a.ownOrigin() {
		p.Username = "admin"
	}
	if r.Method == "POST" {
		p.Username = r.FormValue("username")
		payload := map[string]string{"username": p.Username, "password": r.FormValue("password")}
		endpoint := "/api/login"
		if view == "register" {
			endpoint = "/api/register"
			payload["display"] = r.FormValue("display")
			payload["join_code"] = r.FormValue("join_code")
		}
		if e := s.Remote.call("POST", endpoint, payload, nil); e != nil {
			p.Error = e.Error()
		} else {
			if view == "register" {
				s.Notice = "Account requested. Ask the host to approve you, then sign in."
				http.Redirect(w, r, "/login", 303)
			} else {
				s.Notice = "Signed in."
				a.rotateWeb(w, s, true)
				http.Redirect(w, r, "/chat", 303)
			}
			return
		}
	}
	a.render(w, p)
}
func (a *App) chatAction(w http.ResponseWriter, r *http.Request, s *WebSession, info SessionInfo) {
	destination := "/chat"
	endpoint := ""
	payload := map[string]any{}
	notice := ""
	var err error
	switch r.URL.Path {
	case "/send":
		nonce := r.FormValue("request_id")
		if len(nonce) < 16 || len(nonce) > 64 {
			http.Error(w, "Refresh the chat before sending.", 400)
			return
		}
		if s.Sent[nonce] {
			http.Redirect(w, r, "/chat", 303)
			return
		}
		endpoint = "/api/send"
		payload = map[string]any{"body": r.FormValue("body"), "recipient_id": r.FormValue("recipient_id"), "request_id": nonce}
		notice = "Message sent."
	case "/logout":
		endpoint = "/api/logout"
		destination = "/login"
		notice = "Signed out."
	case "/admin/action":
		if !info.Admin {
			http.Error(w, "Host permission required.", 403)
			return
		}
		action := r.FormValue("action")
		if (strings.HasPrefix(action, "delete-") || action == "history") && r.FormValue("confirm") != "yes" {
			s.Notice = "Confirm permanent deletion first."
			http.Redirect(w, r, "/admin", 303)
			return
		}
		destination = "/admin?tab=" + adminTab(r.FormValue("tab"))
		endpoint = "/api/admin/action"
		for _, key := range []string{"action", "id", "days", "limit_mb", "minutes"} {
			payload[key] = r.FormValue(key)
		}
		notice = "Changes saved."
	case "/admin/password":
		if !info.Admin {
			http.Error(w, "Host permission required.", 403)
			return
		}
		destination = "/login"
		if r.FormValue("new_password") != r.FormValue("confirm_password") {
			s.Notice = "The new passwords do not match."
			http.Redirect(w, r, "/admin?tab=security", 303)
			return
		}
		endpoint = "/api/admin/password"
		payload = map[string]any{"current_password": r.FormValue("current_password"), "new_password": r.FormValue("new_password")}
		notice = "Password changed. Sign in with the new password."
	default:
		http.NotFound(w, r)
		return
	}
	if endpoint != "" {
		err = s.Remote.call("POST", endpoint, payload, nil)
	}
	if err != nil {
		s.Notice = err.Error()
		if r.URL.Path == "/admin/password" {
			destination = "/admin?tab=security"
		}
	} else {
		s.Notice = notice
		if r.URL.Path == "/logout" || r.URL.Path == "/admin/password" {
			a.rotateWeb(w, s, false)
		}
		if r.URL.Path == "/send" {
			if len(s.Sent) > 128 {
				s.Sent = map[string]bool{}
			}
			s.Sent[r.FormValue("request_id")] = true
		}
	}
	http.Redirect(w, r, destination, 303)
}
func adminTab(value string) string {
	switch value {
	case "accounts", "security", "storage", "history":
		return value
	}
	return "requests"
}
func (a *App) fileDownload(w http.ResponseWriter, r *http.Request, s *WebSession) {
	id, e := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/download/"), 10, 64)
	if e != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	response, e := s.Remote.request("GET", "/api/download/"+strconv.FormatInt(id, 10), nil, "")
	if e != nil {
		p := a.basePage(s, "offline")
		p.Error = e.Error()
		a.render(w, p)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "That file is not available to your account.", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	disposition := response.Header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment") {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 100<<20))
}
func (a *App) streamWebUpload(w http.ResponseWriter, r *http.Request, s *WebSession) {
	r.Body = http.MaxBytesReader(w, r.Body, 101<<20)
	reader, e := r.MultipartReader()
	if e != nil {
		http.Error(w, "Invalid upload form.", 400)
		return
	}
	first, e := reader.NextPart()
	if e != nil || first.FormName() != "csrf" {
		http.Error(w, "Refresh the Files page before uploading.", 400)
		return
	}
	token, e := io.ReadAll(io.LimitReader(first, 129))
	if e != nil || len(token) > 128 || !equal(string(token), s.CSRF) {
		http.Error(w, "Invalid upload token.", 400)
		return
	}
	first.Close()
	if s.Remote == nil {
		http.Redirect(w, r, "/home", 303)
		return
	}
	info, e := s.Remote.info()
	if e != nil || !info.Authenticated {
		http.Redirect(w, r, "/login", 303)
		return
	}
	select {
	case a.uiUploads <- struct{}{}:
		defer func() { <-a.uiUploads }()
	default:
		http.Error(w, "Another upload is in progress. Try again shortly.", 429)
		return
	}
	recipient := ""
	found := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.Notice = "The upload was interrupted."
			break
		}
		if part.FormName() == "recipient_id" {
			b, err := io.ReadAll(io.LimitReader(part, 65))
			if err != nil || len(b) > 64 {
				http.Error(w, "Invalid recipient.", 400)
				return
			}
			recipient = string(b)
			part.Close()
			continue
		}
		if part.FormName() == "file" && part.FileName() != "" {
			found = true
			err = s.Remote.upload(part.FileName(), recipient, part, func() { _ = r.Body.Close() })
			if err != nil {
				s.Notice = err.Error()
			} else {
				s.Notice = "File shared."
			}
			break
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(part, 4097))
		part.Close()
	}
	if !found && s.Notice == "" {
		s.Notice = "Choose a file first."
	}
	http.Redirect(w, r, "/files", 303)
}
func addresses() []string {
	out := []string{}
	interfaces, _ := net.Interfaces()
	for _, in := range interfaces {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := in.Addrs()
		for _, addr := range addrs {
			ip, _, e := net.ParseCIDR(addr.String())
			if e == nil && ip.To4() != nil && ip.IsPrivate() {
				out = append(out, "https://"+ip.String()+":8000")
			}
		}
	}
	return out
}
