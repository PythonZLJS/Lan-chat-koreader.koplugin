package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KOReader talks only to this capability-protected loopback bridge. All LAN
// operations run in its worker, never in the reader's UI thread or state GET.
type ReaderState struct {
	Context       string       `json:"context"`
	Discoverable  bool         `json:"discoverable"`
	DiscoveryName string       `json:"discovery_name"`
	Nearby        []NearbyHost `json:"nearby"`
	Scanned       bool         `json:"scanned"`
	Version       int          `json:"version"`
	Stage         string       `json:"stage"`
	Busy          bool         `json:"busy"`
	Paused        bool         `json:"paused"`
	Connected     bool         `json:"connected"`
	Hosting       bool         `json:"hosting"`
	Admin         bool         `json:"admin"`
	VaultLocked   bool         `json:"vault_locked"`
	Address       string       `json:"address"`
	Display       string       `json:"display"`
	Code          string       `json:"code"`
	VerifyToken   string       `json:"verify_token,omitempty"`
	Error         string       `json:"error"`
	Notice        string       `json:"notice"`
	SentToken     string       `json:"sent_token,omitempty"`
	JoinCode      string       `json:"join_code"`
	Addresses     []string     `json:"addresses"`
	Messages      []Message    `json:"messages"`
	Users         []User       `json:"users"`
	Files         []FileInfo   `json:"files"`
	Pending       []User       `json:"pending"`
	More          bool         `json:"more"`
	Before        int64        `json:"before"`
	Revision      uint64       `json:"revision"`
	Recent        []string     `json:"recent_hosts"`
	Room          string       `json:"room"`
	Rooms         []string     `json:"rooms"`
	UserID        int64        `json:"user_id"`
	LatencyMS     int64        `json:"latency_ms"`
	Accounts      []User       `json:"accounts"`
	Inbox         []Message    `json:"inbox"`
	Results       []Message    `json:"results"`
	Query         string       `json:"query"`
}
type ReaderAction struct {
	Context   string `json:"context,omitempty"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	Address   string `json:"address"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Confirm   string `json:"confirm"`
	Display   string `json:"display"`
	JoinCode  string `json:"join_code"`
	Body      string `json:"body"`
	Recipient string `json:"recipient"`
	Token     string `json:"token"`
	ID        int64  `json:"id"`
	Before    int64  `json:"before"`
	Path      string `json:"path"`
	Room      string `json:"room"`
	Query     string `json:"query"`
}
type ReaderBridge struct {
	app          *App
	token        string
	mu           sync.Mutex
	state        ReaderState
	jobs         chan ReaderAction
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	closeOnce    sync.Once
	remote       *Remote // worker-owned; state never exposes cookies or credentials
	pending      *PendingHost
	before       int64
	lastViewed   time.Time
	usersAt      time.Time
	fullAt       time.Time
	forceRefresh bool
}

func newReaderBridge(a *App) (*ReaderBridge, error) {
	path := filepath.Join(a.Store.Data, "koreader-token.txt")
	token := readText(path)
	if token == "" {
		if _, err := os.Stat(path); err == nil {
			return nil, errors.New("the KOReader access key is empty; restore it or remove that empty file while stopped")
		}
		token = randomToken(32)
		if err := atomicWrite(path, []byte(token), 0600); err != nil {
			return nil, err
		}
	}
	if len(token) != 43 || strings.ContainsAny(token, "\r\n ") {
		return nil, errors.New("the KOReader access key is damaged")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &ReaderBridge{app: a, token: token, jobs: make(chan ReaderAction, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), state: ReaderState{Context: randomToken(16), Version: 4, Stage: "idle"}}
	_, p.state.VaultLocked = a.Store.Vault.status()
	go p.loop()
	return p, nil
}
func (p *ReaderBridge) close() {
	p.closeOnce.Do(func() { p.cancel() })
	<-p.done
}
func (p *ReaderBridge) change(fn func(*ReaderState)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.state)
	p.state.Revision++
}
func (p *ReaderBridge) snapshot() ReaderState {
	p.mu.Lock()
	s := p.state
	s.Nearby = append([]NearbyHost(nil), p.state.Nearby...)
	s.Messages = append([]Message(nil), p.state.Messages...)
	s.Users = append([]User(nil), p.state.Users...)
	s.Files = append([]FileInfo(nil), p.state.Files...)
	s.Pending = append([]User(nil), p.state.Pending...)
	s.Accounts = append([]User(nil), p.state.Accounts...)
	s.Inbox = append([]Message(nil), p.state.Inbox...)
	s.Results = append([]Message(nil), p.state.Results...)
	s.Addresses = append([]string(nil), p.state.Addresses...)
	s.Rooms = append([]string(nil), p.state.Rooms...)
	s.Recent = append([]string(nil), p.state.Recent...)
	p.mu.Unlock()
	s.Hosting = p.app.hosting()
	s.Discoverable, s.DiscoveryName = p.app.discoveryStatus()
	p.app.savedMu.Lock()
	s.Recent = []string{}
	for _, host := range p.app.saved {
		s.Recent = append(s.Recent, host.Origin)
	}
	p.app.savedMu.Unlock()
	return s
}
func (p *ReaderBridge) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || !equal(r.Header.Get("Authorization"), "Bearer "+p.token) {
		apiError(w, http.StatusForbidden, "Open Neighbour Chat from KOReader.")
		return
	}
	if r.URL.Path == "/_reader/state" && r.Method == "GET" {
		p.mu.Lock()
		p.lastViewed = time.Now()
		p.mu.Unlock()
		state := p.snapshot()
		etag := fmt.Sprintf("\"%d-%t-%t\"", state.Revision, state.Hosting, state.Discoverable)
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		payload, err := readerStateJSON(state)
		if err != nil {
			apiError(w, 500, "Could not encode chat state.")
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(payload)
		return
	}
	if r.URL.Path != "/_reader/action" || r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	var action ReaderAction
	if !decodeJSON(w, r, &action) {
		return
	}
	if action.Action == "stop" {
		writeJSON(w, 200, map[string]bool{"stopping": true})
		if p.app.shutdown != nil {
			p.app.shutdownOnce.Do(func() { close(p.app.shutdown) })
		}
		return
	}
	if action.Action == "pause" || action.Action == "resume" {
		p.change(func(s *ReaderState) { s.Paused = action.Action == "pause" })
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	switch action.Action {
	case "scan", "join-nearby", "discovery-on", "discovery-off", "host", "probe", "confirm", "login", "register", "refresh", "older", "latest", "send", "disconnect", "end-chat", "stop-host", "invite", "admin", "approve", "deny", "unlock", "lock", "files", "upload", "download", "reconnect", "forget", "people", "create-room", "delete-room", "move-user", "kick", "ban", "unban", "inbox", "search":
	default:
		apiError(w, 400, "Unknown chat action.")
		return
	}
	p.mu.Lock()
	if p.state.Busy {
		p.mu.Unlock()
		apiError(w, 409, "Another chat operation is finishing. Try again shortly.")
		return
	}
	p.state.Busy = true
	p.state.Error = ""
	p.state.Notice = ""
	p.state.Revision++
	p.mu.Unlock()
	select {
	case p.jobs <- action:
		writeJSON(w, 202, map[string]bool{"accepted": true})
	case <-p.ctx.Done():
		apiError(w, 503, "Chat has stopped.")
	}
}
func (p *ReaderBridge) loop() {
	defer close(p.done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer func() {
		if p.remote != nil {
			p.remote.Client.CloseIdleConnections()
		}
	}()
	failures := 0
	nextPoll := time.Now()
	for {
		select {
		case <-p.ctx.Done():
			return
		case job := <-p.jobs:
			if p.ctx.Err() != nil {
				return
			}
			err := p.safeExecute(job)
			p.change(func(s *ReaderState) {
				s.Busy = false
				if err != nil {
					s.Error = truncate(err.Error(), 240)
				}
			})
			failures = 0
			nextPoll = time.Now().Add(p.pollInterval())
		case <-ticker.C:
			state := p.snapshot()
			if p.remote == nil || state.Paused || !state.Connected || time.Now().Before(nextPoll) {
				continue
			}
			// Back off on a sleeping/offline host, retaining the authenticated jar.
			if err := p.refresh(); err != nil {
				failures++
				p.change(func(s *ReaderState) { s.Error = "Reconnecting: " + truncate(err.Error(), 200) })
				delay := time.Duration(failures) * 20 * time.Second
				if delay > 60*time.Second {
					delay = 60 * time.Second
				}
				nextPoll = time.Now().Add(delay)
			} else {
				failures = 0
				nextPoll = time.Now().Add(p.pollInterval())
			}
		}
	}
}
func (p *ReaderBridge) safeExecute(job ReaderAction) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("chat operation failed safely")
		}
	}()
	return p.execute(job)
}
func (p *ReaderBridge) pollInterval() time.Duration {
	p.mu.Lock()
	last := p.lastViewed
	p.mu.Unlock()
	if time.Since(last) < 5*time.Second {
		return 10 * time.Second
	}
	return 30 * time.Second
}
func (p *ReaderBridge) setRemote(r *Remote) {
	if p.remote != nil {
		p.remote.Client.CloseIdleConnections()
	}
	p.remote = r
	if r != nil {
		r.Ctx = p.ctx
		r.Client.Timeout = 12 * time.Second
	}
}
func (p *ReaderBridge) clear() {
	p.setRemote(nil)
	p.pending = nil
	p.before = 0
	p.usersAt = time.Time{}
	p.fullAt = time.Time{}
	p.forceRefresh = true
	p.change(func(s *ReaderState) {
		*s = ReaderState{Context: randomToken(16), Version: 4, Stage: "idle", Busy: s.Busy, Revision: s.Revision}
	})
}
func (p *ReaderBridge) refresh() error {
	if p.remote == nil {
		return errors.New("Connect to a host first.")
	}
	cached := p.snapshot()
	info := SessionInfo{Authenticated: cached.Connected, Admin: cached.Admin, Display: cached.Display, UserID: cached.UserID}
	var err error
	if !cached.Connected {
		info, err = p.remote.info()
		if err != nil {
			return err
		}
	}
	if !info.Authenticated {
		p.signedOut()
		return nil
	}
	info.Display = truncate(info.Display, 40)
	var data ChatData
	full := p.forceRefresh || time.Since(p.fullAt) > time.Minute
	usersInterval := time.Minute
	if p.pollInterval() == 10*time.Second {
		usersInterval = 10 * time.Second
	}
	loadUsers := full || time.Since(p.usersAt) > usersInterval
	incremental := p.before == 0 && !full && len(cached.Messages) > 0
	q := "/api/chat-data?section=chat&page_size=12"
	if cached.Room != "" {
		q += "&room_hint=" + urlQuery(cached.Room)
	}
	if cached.LatencyMS >= 0 {
		q += "&rtt_ms=" + strconv.FormatInt(cached.LatencyMS, 10)
	}
	if !loadUsers {
		q += "&include_users=0"
	}
	if p.before > 0 {
		q += "&before=" + strconv.FormatInt(p.before, 10)
	} else if incremental {
		q += "&message_after=" + strconv.FormatInt(cached.Messages[0].ID, 10)
	}
	started := time.Now()
	if err = p.remote.call("GET", q, nil, &data); err != nil {
		var status *remoteStatusError
		if errors.As(err, &status) && status.Status == 401 {
			p.signedOut()
			return nil
		}
		return err
	}
	latency := time.Since(started).Milliseconds()
	if data.RoomChanged || (cached.Room != "" && data.Room != "" && cached.Room != data.Room) {
		incremental = false
		p.before = 0
		p.fullAt = time.Time{}
	}
	if len(data.Messages) > 12 {
		data.Messages = data.Messages[len(data.Messages)-12:]
	}
	if len(data.Users) > 500 {
		data.Users = data.Users[:500]
	}
	data.Messages = readerMessages(data.Messages, 12)
	for i := range data.Users {
		data.Users[i].Display = truncate(data.Users[i].Display, 40)
		data.Users[i].Username = truncate(data.Users[i].Username, 32)
	}
	for i, j := 0, len(data.Messages)-1; i < j; i, j = i+1, j-1 {
		data.Messages[i], data.Messages[j] = data.Messages[j], data.Messages[i]
	}
	if incremental {
		data.Messages = append(data.Messages, cached.Messages...)
		data.More = data.More || cached.More || len(data.Messages) > 12
		if len(data.Messages) > 12 {
			data.Messages = data.Messages[:12]
		}
	}
	var before int64
	if len(data.Messages) > 0 {
		before = data.Messages[len(data.Messages)-1].ID
	}
	if loadUsers {
		p.usersAt = time.Now()
	} else {
		data.Users = cached.Users
	}
	if full {
		p.fullAt = time.Now()
	}
	p.forceRefresh = false
	p.change(func(s *ReaderState) {
		s.Stage = "connected"
		s.Connected = true
		s.Admin = info.Admin
		s.Display = info.Display
		s.UserID = info.UserID
		s.Room = data.Room
		s.Rooms = data.Rooms
		s.LatencyMS = latency
		if data.RoomChanged || cached.Room != data.Room {
			if cached.Room != "" {
				s.Context = randomToken(16)
			}
			s.Results = nil
			s.Query = ""
			s.Files = nil
		}
		s.Messages = data.Messages
		s.Users = data.Users
		s.More = data.More
		s.Before = before
		s.Error = ""
	})
	return nil
}
func (p *ReaderBridge) signedOut() {
	p.before = 0
	p.usersAt = time.Time{}
	p.fullAt = time.Time{}
	p.forceRefresh = true
	p.change(func(s *ReaderState) {
		s.Context = randomToken(16)
		s.UserID = 0
		s.Display = ""
		s.Before = 0
		s.More = false
		s.SentToken = ""
		s.Stage = "login"
		s.Connected = false
		s.Admin = false
		s.Messages = nil
		s.Users = nil
		s.Files = nil
		s.Pending = nil
		s.Accounts = nil
		s.Inbox = nil
		s.Results = nil
		s.Query = ""
		s.Rooms = nil
		s.Room = ""
		s.Error = ""
	})
}
func (p *ReaderBridge) execute(v ReaderAction) error {
	if v.Context != "" && !equal(v.Context, p.snapshot().Context) {
		return errors.New("The chat or account changed. Reopen this screen before trying again.")
	}
	a := p.app
	switch v.Action {
	case "scan":
		p.change(func(s *ReaderState) { s.Nearby = nil; s.Scanned = false })
		hosts, err := scanNearby(p.ctx)
		if err != nil {
			return err
		}
		p.change(func(s *ReaderState) {
			s.Nearby = hosts
			s.Scanned = true
			if len(hosts) == 0 {
				s.Notice = "No discoverable chats found. Ask the host to turn on discovery and check you share the same Wi-Fi."
			}
		})
		return nil
	case "join-nearby":
		found := false
		for _, host := range p.snapshot().Nearby {
			if host.Address == v.Address {
				found = true
				break
			}
		}
		if !found {
			return errors.New("Choose a chat from a fresh discovery scan.")
		}
		a.savedMu.Lock()
		known := false
		for _, h := range a.saved {
			if h.Origin == v.Address {
				known = true
				break
			}
		}
		a.savedMu.Unlock()
		if known {
			return p.execute(ReaderAction{Action: "reconnect", Address: v.Address})
		}
		return p.execute(ReaderAction{Action: "probe", Address: v.Address})
	case "discovery-on", "discovery-off":
		enabled := v.Action == "discovery-on"
		if err := a.setDiscovery(enabled, v.Name); err != nil {
			return err
		}
		p.change(func(s *ReaderState) {
			if enabled {
				s.Notice = "Discovery is on. People on your Wi-Fi can find this chat."
			} else {
				s.Notice = "Discovery is off. Existing connections keep working."
			}
		})
		return nil
	case "unlock":
		if err := a.Store.unlockEncryption(v.Password); err != nil {
			return err
		}
		p.change(func(s *ReaderState) {
			s.VaultLocked = false
			s.Stage = "idle"
			s.Notice = "Saved content unlocked. Choose Host or Join."
		})
		return nil
	case "lock":
		p.clear()
		a.stopHost()
		a.Store.Vault.lock()
		p.change(func(s *ReaderState) { s.Stage = "locked"; s.VaultLocked = true })
		return nil
	case "host":
		cached := p.snapshot()
		if cached.Hosting && cached.Address == "This Kindle" && cached.Connected && cached.Admin {
			return p.refresh()
		}
		p.clear()
		_, locked := a.Store.Vault.status()
		if locked {
			p.change(func(s *ReaderState) { s.Stage = "locked"; s.VaultLocked = true })
			return nil
		}
		if err := a.startHost(); err != nil {
			return err
		}
		remote, err := a.ownerRemote()
		if err != nil {
			return err
		}
		p.setRemote(remote)
		p.before = 0
		p.change(func(s *ReaderState) {
			s.Address = "This Kindle"
			s.Notice = "You are the host and admin. Turn on discovery when you want people to find this chat."
		})
		return p.refresh()
	case "probe":
		origin, err := privateOrigin(v.Address)
		if err != nil {
			return err
		}
		pin, err := probeCertificate(origin)
		if err != nil {
			return err
		}
		p.clear()
		p.pending = &PendingHost{Origin: origin, Pin: pin, Expires: time.Now().Add(5 * time.Minute)}
		p.change(func(s *ReaderState) {
			s.Stage = "verify"
			s.Address = origin
			s.Code = checkCode(pin)
			s.VerifyToken = pin
		})
		return nil
	case "confirm":
		if p.pending == nil || time.Now().After(p.pending.Expires) {
			return errors.New("The host check expired. Enter its address again.")
		}
		if !equal(v.Token, p.pending.Pin) {
			return errors.New("The host verification has changed. Check the code again.")
		}
		p.setRemote(newRemote(p.pending.Origin, p.pending.Pin))
		p.before = 0
		if _, err := p.remote.info(); err != nil {
			return err
		}
		if err := a.remember(p.pending.Origin, p.pending.Pin); err != nil {
			return err
		}
		p.pending = nil
		p.change(func(s *ReaderState) { s.Stage = "login"; s.Code = ""; s.VerifyToken = "" })
		return nil
	case "login":
		if p.remote == nil {
			return errors.New("Choose and verify a host first.")
		}
		if _, err := p.remote.info(); err != nil {
			return err
		}
		if err := p.remote.call("POST", "/api/login", map[string]string{"username": v.Username, "password": v.Password}, nil); err != nil {
			return err
		}
		p.signedOut() // A successful login must never merge another account's cached private data.
		if p.snapshot().Address != "This Kindle" {
			if err := a.remember(p.remote.Origin, p.remote.Pin); err != nil {
				return err
			}
		}
		return p.refresh()
	case "register":
		if p.remote == nil {
			return errors.New("Choose and verify a host first.")
		}
		if _, err := p.remote.info(); err != nil {
			return err
		}
		err := p.remote.call("POST", "/api/register", map[string]string{"username": v.Username, "display": v.Display, "password": v.Password, "join_code": v.JoinCode}, nil)
		if err == nil {
			p.change(func(s *ReaderState) { s.Notice = "Request sent. Ask the host to approve you, then sign in." })
		}
		return err
	case "send":
		if p.remote == nil {
			return errors.New("Sign in first.")
		}
		if len(v.Token) < 16 || len(v.Token) > 64 {
			return errors.New("The message retry key is missing. Reopen Write and try again.")
		}
		var recipient any
		if v.Recipient != "" {
			id, err := strconv.ParseInt(v.Recipient, 10, 64)
			if err != nil || id < 1 {
				return errors.New("Choose a valid recipient.")
			}
			recipient = id
		}
		if err := p.remote.call("POST", "/api/send", map[string]any{"body": v.Body, "recipient_id": recipient, "request_id": v.Token, "room_hint": p.snapshot().Room}, nil); err != nil {
			return err
		}
		p.before = 0
		p.forceRefresh = true
		if err := p.refresh(); err != nil {
			return err
		}
		p.change(func(s *ReaderState) { s.Notice = "Message sent."; s.SentToken = v.Token })
		return nil
	case "older":
		p.forceRefresh = true
		p.before = v.Before
		return p.refresh()
	case "latest":
		p.forceRefresh = true
		p.before = 0
		return p.refresh()
	case "refresh":
		p.forceRefresh = true
		return p.refresh()
	case "disconnect":
		if p.remote != nil {
			if err := p.remote.call("POST", "/api/logout", map[string]bool{}, nil); err != nil {
				return fmt.Errorf("disconnect could not be completed: %w", err)
			}
		}
		p.clear()
		return nil
	case "end-chat":
		wasOwnHost := p.snapshot().Address == "This Kindle"
		if p.remote != nil {
			if err := p.remote.call("POST", "/api/logout", map[string]bool{}, nil); err != nil {
				return fmt.Errorf("end chat could not log out safely: %w", err)
			}
		}
		if wasOwnHost {
			a.stopHost()
		}
		p.clear()
		p.change(func(s *ReaderState) {
			s.Notice = "Chat ended. Saved messages and backups were kept."
		})
		return nil
	case "stop-host":
		a.stopHost()
		if p.remote != nil && p.snapshot().Address == "This Kindle" {
			p.clear()
		}
		return nil
	case "invite":
		if !a.hosting() {
			return errors.New("Start hosting on this Kindle first.")
		}
		p.change(func(s *ReaderState) {
			s.JoinCode = a.Store.joinCode()
			s.Code = checkCode(a.Store.Fingerprint)
			s.Addresses = addresses()
		})
		return nil
	case "admin":
		return p.loadHost()
	case "people":
		p.forceRefresh = true
		return p.refresh()
	case "reconnect":
		origin, err := privateOrigin(v.Address)
		if err != nil {
			return err
		}
		a.savedMu.Lock()
		var saved *SavedHost
		for _, h := range a.saved {
			if h.Origin == origin {
				copy := h
				saved = &copy
				break
			}
		}
		a.savedMu.Unlock()
		if saved == nil {
			return errors.New("This host is not saved. Add its address and verify the code first.")
		}
		p.clear()
		p.setRemote(newRemote(saved.Origin, saved.Pin))
		if _, err = p.remote.info(); err != nil {
			return err
		}
		if err = a.remember(saved.Origin, saved.Pin); err != nil {
			return err
		}
		p.change(func(s *ReaderState) {
			s.Stage = "login"
			s.Address = saved.Origin
			s.Notice = "Saved host verified. Sign in to reconnect."
		})
		return nil
	case "forget":
		origin, err := privateOrigin(v.Address)
		if err != nil {
			return err
		}
		if err = a.forgetHost(origin); err != nil {
			return err
		}
		p.change(func(s *ReaderState) { s.Notice = "Address removed from recent hosts." })
		return nil
	case "create-room", "delete-room", "move-user", "kick", "ban", "unban":
		if p.remote == nil {
			return errors.New("Sign in as the host first.")
		}
		if err := p.remote.call("POST", "/api/host/action", map[string]any{"action": v.Action, "id": v.ID, "room": v.Room}, nil); err != nil {
			return err
		}
		if err := p.loadHost(); err != nil {
			return err
		}
		p.forceRefresh = true
		return p.refresh()
	case "inbox":
		if p.remote == nil {
			return errors.New("Sign in as the host first.")
		}
		var data struct {
			Messages []Message `json:"messages"`
		}
		if err := p.remote.call("GET", "/api/inbox", nil, &data); err != nil {
			return err
		}
		messages := readerMessages(data.Messages, 50)
		p.change(func(s *ReaderState) { s.Inbox = messages })
		return nil
	case "search":
		if p.remote == nil {
			return errors.New("Sign in first.")
		}
		var data struct {
			Results []Message `json:"results"`
		}
		if err := p.remote.call("GET", "/api/search?q="+urlQuery(v.Query), nil, &data); err != nil {
			return err
		}
		results := readerMessages(data.Results, 100)
		p.change(func(s *ReaderState) { s.Query = v.Query; s.Results = results })
		return nil
	case "approve", "deny":
		if p.remote == nil {
			return errors.New("Sign in as the host first.")
		}
		action := "approve"
		if v.Action == "deny" {
			action = "disable"
		}
		if err := p.remote.call("POST", "/api/admin/action", map[string]any{"action": action, "id": v.ID}, nil); err != nil {
			return err
		}
		return p.loadHost()
	case "files":
		if p.remote == nil {
			return errors.New("Sign in first.")
		}
		var data ChatData
		if err := p.remote.call("GET", "/api/chat-data?section=files", nil, &data); err != nil {
			return err
		}
		if len(data.Files) > 50 {
			data.Files = data.Files[:50]
		}
		for i := range data.Files {
			data.Files[i].Name = truncate(data.Files[i].Name, 255)
			data.Files[i].Display = truncate(data.Files[i].Display, 40)
		}
		p.change(func(s *ReaderState) { s.Files = data.Files })
		return nil
	case "upload":
		if p.remote == nil {
			return errors.New("Sign in first.")
		}
		p.remote.Client.Timeout = 120 * time.Second
		defer func() { p.remote.Client.Timeout = 12 * time.Second }()
		path, err := filepath.EvalSymlinks(v.Path)
		if err != nil {
			return err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return err
		}
		storage, err := filepath.EvalSymlinks(filepath.Dir(a.Store.Root))
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(storage, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Choose a file in Kindle Internal Storage.")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 100<<20 {
			return errors.New("Choose a regular file no larger than 100 MB; the host's limit may be lower.")
		}
		if err = p.remote.upload(filepath.Base(path), v.Recipient, file, func() { file.Close() }, p.snapshot().Room); err != nil {
			return err
		}
		p.change(func(s *ReaderState) { s.Notice = "File shared." })
		return nil
	case "download":
		if p.remote == nil || v.ID < 1 {
			return errors.New("Choose an available file.")
		}
		p.remote.Client.Timeout = 120 * time.Second
		defer func() { p.remote.Client.Timeout = 12 * time.Second }()
		var chosen *FileInfo
		for _, file := range p.snapshot().Files {
			if file.ID == v.ID {
				copy := file
				chosen = &copy
				break
			}
		}
		if chosen == nil {
			return errors.New("Refresh the file list first.")
		}
		if chosen.Size < 0 || chosen.Size > 100<<20 {
			return errors.New("This file is too large to download here.")
		}
		response, err := p.remote.request("GET", "/api/download/"+strconv.FormatInt(v.ID, 10), nil, "")
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("The file is no longer available to this account.")
		}
		folder := filepath.Join(a.Store.Data, "downloads")
		if err = privateDirectory(folder); err != nil {
			return err
		}
		name := strings.Map(func(r rune) rune {
			if r == '/' || r == '\\' || r < 32 || r == 127 {
				return '_'
			}
			return r
		}, chosen.Name)
		name = truncate(name, 100)
		if name == "" || name == "." || name == ".." {
			name = "download"
		}
		target := filepath.Join(folder, randomToken(8)+"-"+name)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		complete := false
		defer func() {
			file.Close()
			if !complete {
				os.Remove(target)
			}
		}()
		n, err := io.Copy(file, io.LimitReader(response.Body, chosen.Size+1))
		if err != nil {
			return err
		}
		if n != chosen.Size {
			return errors.New("The file transfer was incomplete or changed size. Try again.")
		}
		if err = file.Sync(); err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		complete = true
		p.change(func(s *ReaderState) { s.Notice = "Saved to data/downloads/" + filepath.Base(target) })
		return nil
	}
	return errors.New("Unknown chat operation.")
}

func readerMessages(messages []Message, limit int) []Message {
	if len(messages) > limit {
		messages = messages[:limit]
	}
	for i := range messages {
		messages[i].Body = truncate(messages[i].Body, 4000)
		messages[i].Display = truncate(messages[i].Display, 40)
		messages[i].RecipientDisplay = truncate(messages[i].RecipientDisplay, 40)
		messages[i].Room = truncate(messages[i].Room, 64)
	}
	return messages
}
func (p *ReaderBridge) loadHost() error {
	if p.remote == nil {
		return errors.New("Sign in as the host first.")
	}
	var data HostData
	if err := p.remote.call("GET", "/api/host", nil, &data); err != nil {
		return err
	}
	if len(data.Users) > 500 {
		data.Users = data.Users[:500]
	}
	if len(data.Pending) > 500 {
		data.Pending = data.Pending[:500]
	}
	if len(data.Rooms) > 100 {
		data.Rooms = data.Rooms[:100]
	}
	for _, list := range [][]User{data.Users, data.Pending} {
		for i := range list {
			list[i].Display = truncate(list[i].Display, 40)
			list[i].Username = truncate(list[i].Username, 32)
			list[i].Room = truncate(list[i].Room, 64)
			list[i].IP = truncate(list[i].IP, 64)
		}
	}
	p.change(func(s *ReaderState) { s.Accounts = data.Users; s.Pending = data.Pending; s.Rooms = data.Rooms })
	return nil
}

// The Lua transport intentionally caps replies at 512 KiB. Combined search,
// inbox and account caches must not make the local UI permanently unreadable.
// Trim only the returned preview: authoritative stored messages are untouched.
func readerStateJSON(s ReaderState) ([]byte, error) {
	const budget = 480 << 10
	for {
		payload, err := json.Marshal(s)
		if err != nil || len(payload) <= budget {
			return payload, err
		}
		s.Notice = "Large view trimmed to fit the reader. Refine search or refresh People/Manage for another view."
		switch {
		case len(s.Results) > 1:
			s.Results = s.Results[:(len(s.Results)+1)/2]
		case len(s.Inbox) > 1:
			s.Inbox = s.Inbox[:(len(s.Inbox)+1)/2]
		case len(s.Messages) > 1:
			s.Messages = s.Messages[:(len(s.Messages)+1)/2]
			s.More = true
			s.Before = s.Messages[len(s.Messages)-1].ID
		case len(s.Accounts) > 1:
			s.Accounts = s.Accounts[:(len(s.Accounts)+1)/2]
		case len(s.Users) > 1:
			s.Users = s.Users[:(len(s.Users)+1)/2]
		case len(s.Pending) > 1:
			s.Pending = s.Pending[:(len(s.Pending)+1)/2]
		default:
			return nil, errors.New("reader state exceeds response limit")
		}
	}
}
