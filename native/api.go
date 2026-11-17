package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxFileBytes = int64(25 << 20)
const maxStoredBytes = int64(256 << 20)

var validUsername = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Display   string `json:"display_name"`
	Status    string `json:"status,omitempty"`
	Created   string `json:"created_at,omitempty"`
	Room      string `json:"room"`
	Online    bool   `json:"online"`
	LatencyMS int64  `json:"latency_ms"`
	IP        string `json:"ip,omitempty"`
}
type Message struct {
	Encrypted        int    `json:"-"`
	ID               int64  `json:"id"`
	Body             string `json:"body"`
	Created          string `json:"created_at"`
	Recipient        *int64 `json:"recipient_id"`
	Display          string `json:"display_name"`
	UserID           int64  `json:"user_id"`
	Room             string `json:"room"`
	RecipientDisplay string `json:"recipient_display,omitempty"`
}
type FileInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"original_name"`
	Size      int64  `json:"size"`
	Created   string `json:"created_at"`
	Recipient *int64 `json:"recipient_id"`
	Display   string `json:"display_name"`
}
type API struct {
	Store       *Store
	Sessions    APISessions
	Rate        Limiter
	mu          sync.Mutex
	UploadLimit int64
	LimitUntil  time.Time
	uploads     chan struct{}
	presence    map[int64]Presence
}

func newAPI(s *Store) *API {
	return &API{Store: s, UploadLimit: maxFileBytes, uploads: make(chan struct{}, 2)}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	if e := d.Decode(v); e != nil {
		apiError(w, 400, "Invalid or oversized request.")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		apiError(w, 400, "Only one JSON object is allowed.")
		return false
	}
	return true
}
func intValue(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		n, e := strconv.ParseInt(x, 10, 64)
		return n, e == nil && n > 0
	case float64:
		n := int64(x)
		return n, x == float64(n) && n > 0
	case json.Number:
		n, e := x.Int64()
		return n, e == nil && n > 0
	}
	return 0, false
}
func (a *API) authorized(s APISession) bool {
	if s.UID == 0 {
		return false
	}
	if s.Admin {
		_, version := a.Store.adminState()
		return s.Version == version
	}
	var status string
	var version int64
	return a.Store.DB.QueryRow("SELECT status,auth_version FROM users WHERE id=?", s.UID).Scan(&status, &version) == nil && status == "approved" && s.Version == version
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if r.TLS == nil {
		apiError(w, 403, "HTTPS is required.")
		return
	}
	if a.ipBanned(clientIP(r)) {
		apiError(w, 403, "This IP address has been banned by the host.")
		return
	}
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		apiError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	if r.Method == "POST" {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host {
			apiError(w, http.StatusForbidden, "Invalid request origin.")
			return
		}
	}
	a.Store.Vault.mu.RLock()
	defer a.Store.Vault.mu.RUnlock()
	if !a.Store.Vault.ready() {
		apiError(w, 423, "Unlock saved-data encryption on this Kindle's Home screen first.")
		return
	}
	s, ok := a.Sessions.get(w, r)
	if !ok {
		return
	}
	if r.Method == "POST" && !equal(r.Header.Get("X-CSRF-Token"), s.CSRF) {
		apiError(w, 400, "Refresh and try again.")
		return
	}
	p := r.URL.Path
	if p == "/api/session" && r.Method == "GET" {
		result := map[string]any{"csrf": s.CSRF, "authenticated": false, "version": 3}
		if a.authorized(s) {
			var display string
			_ = a.Store.DB.QueryRow("SELECT display_name FROM users WHERE id=?", s.UID).Scan(&display)
			result["authenticated"] = true
			result["display"] = display
			result["admin"] = s.Admin
			result["user_id"] = s.UID
		}
		writeJSON(w, 200, result)
		return
	}
	if p == "/api/login" && r.Method == "POST" {
		a.login(w, r, s)
		return
	}
	if p == "/api/register" && r.Method == "POST" {
		a.register(w, r)
		return
	}
	if !a.authorized(s) {
		apiError(w, 401, "Sign in to continue.")
		return
	}
	a.touch(s, r)
	switch {
	case p == "/api/host" && r.Method == "GET" && s.Admin:
		a.hostData(w, r)
	case p == "/api/host/action" && r.Method == "POST" && s.Admin:
		a.collaborationAction(w, r)
	case p == "/api/inbox" && r.Method == "GET" && s.Admin:
		a.inbox(w, r)
	case p == "/api/logout" && r.Method == "POST":
		a.Sessions.revoke(s.ID)
		a.mu.Lock()
		delete(a.presence, s.UID)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "nc_api", Value: "", Path: "/", HttpOnly: true, Secure: true, MaxAge: -1})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case p == "/api/chat-data" && r.Method == "GET":
		a.chatData(w, r, s)
	case p == "/api/search" && r.Method == "GET":
		a.search(w, r, s)
	case p == "/api/send" && r.Method == "POST":
		a.send(w, r, s)
	case p == "/api/upload" && r.Method == "POST":
		a.upload(w, r, s)
	case strings.HasPrefix(p, "/api/download/") && r.Method == "GET":
		a.download(w, r, s)
	case p == "/api/admin" && r.Method == "GET" && s.Admin:
		a.adminData(w, r)
	case p == "/api/admin/action" && r.Method == "POST" && s.Admin:
		a.adminAction(w, r)
	default:
		apiError(w, 404, "This action is not available.")
	}
}
func (a *API) login(w http.ResponseWriter, r *http.Request, s APISession) {
	var v struct{ Username, Password string }
	if !decodeJSON(w, r, &v) {
		return
	}
	v.Username = strings.ToLower(strings.TrimSpace(v.Username))
	if !validPasswordInput(v.Password) || len(v.Username) > 32 {
		apiError(w, 400, "Check the username and password.")
		return
	}
	if !a.Rate.Allow("login-ip:"+clientIP(r), 30, 10*time.Minute) || !a.Rate.Allow("login-name:"+v.Username, 15, 10*time.Minute) {
		apiError(w, 429, "Too many sign-in attempts. Wait 10 minutes.")
		return
	}
	// Host ownership comes only from the server's local controller capability.
	// There is no host password or remotely claimable admin login.
	if v.Username == "admin" {
		apiError(w, 403, "The server owner controls host tools on the Kindle running this server.")
		return
	}
	_, version := a.Store.adminState()
	var hash string
	var uid int64
	var display, status string
	admin := false
	_ = a.Store.DB.QueryRow("SELECT id,display_name,status,password_hash,auth_version FROM users WHERE username=? AND username!=?", v.Username, adminUsername).Scan(&uid, &display, &status, &hash, &version)
	verified := verifyPassword(hash, v.Password)
	if !verified || uid == 0 {
		apiError(w, 401, "Incorrect username or password.")
		return
	}
	if !admin && status != "approved" {
		apiError(w, 403, "Your account is pending approval or has been disabled.")
		return
	}
	next, rotated := a.Sessions.rotate(w, s, uid, admin, version)
	if !rotated {
		apiError(w, 409, "This session changed. Refresh before signing in again.")
		return
	}
	writeJSON(w, 200, map[string]any{"authenticated": true, "display": display, "admin": admin, "csrf": next.CSRF})
}
func (a *API) register(w http.ResponseWriter, r *http.Request) {
	if !a.Rate.Allow("register:"+clientIP(r), 5, time.Hour) {
		apiError(w, 429, "Too many requests. Try again in an hour.")
		return
	}
	var v struct {
		Username, Password, Display string
		Join                        string `json:"join_code"`
	}
	if !decodeJSON(w, r, &v) {
		return
	}
	v.Username = strings.ToLower(strings.TrimSpace(v.Username))
	v.Display = strings.TrimSpace(v.Display)
	if !equal(strings.ToUpper(strings.TrimSpace(v.Join)), a.Store.joinCode()) || !validUsername.MatchString(v.Username) || v.Username == "admin" || v.Username == adminUsername || v.Display == "" || utf8.RuneCountInString(v.Display) > 40 || !validPassword(v.Password) {
		apiError(w, 400, "Check the join code and details. Passwords need 10 to 256 characters.")
		return
	}
	hash, e := hashPassword(v.Password)
	if e != nil {
		apiError(w, 500, "Could not save the account.")
		return
	}
	_, e = a.Store.DB.Exec("INSERT INTO users(username,display_name,password_hash,created_at) VALUES(?,?,?,?)", v.Username, v.Display, hash, timestamp())
	if e != nil {
		apiError(w, 409, "That username is already in use.")
		return
	}
	writeJSON(w, 201, map[string]string{"message": "Request sent. Ask the host to approve your account."})
}
func (a *API) recipient(v any) (any, error) {
	if v == nil || v == "" {
		return nil, nil
	}
	id, ok := intValue(v)
	if !ok {
		return nil, errors.New("Choose an available recipient.")
	}
	var present int
	if a.Store.DB.QueryRow("SELECT 1 FROM users WHERE id=? AND status='approved'", id).Scan(&present) != nil {
		return nil, errors.New("That recipient is not available.")
	}
	return id, nil
}
func (a *API) send(w http.ResponseWriter, r *http.Request, s APISession) {
	if !a.Rate.Allow(fmt.Sprint("send:", s.UID), 60, time.Minute) {
		apiError(w, 429, "Please wait before sending more messages.")
		return
	}
	var v struct {
		Body      string
		Recipient any     `json:"recipient_id"`
		RoomHint  *string `json:"room_hint"`
		Token     string  `json:"request_id"`
	}
	if !decodeJSON(w, r, &v) {
		return
	}
	v.Body = strings.TrimSpace(v.Body)
	if v.Body == "" || utf8.RuneCountInString(v.Body) > 4000 {
		apiError(w, 400, "Write a message of 1 to 4,000 characters.")
		return
	}
	if v.Token == "" {
		v.Token = randomToken(24)
	}
	if len(v.Token) > 64 {
		apiError(w, 400, "Invalid message token.")
		return
	}
	parts := strings.Fields(v.Body)
	if (v.Recipient == nil || v.Recipient == "") && len(parts) > 0 && (strings.EqualFold(parts[0], "@admin") || strings.EqualFold(parts[0], "/admin")) {
		id, err := a.adminTarget()
		if err != nil {
			apiError(w, 500, "Host account missing.")
			return
		}
		v.Recipient = strconv.FormatInt(id, 10)
		v.Body = strings.TrimSpace(v.Body[len(parts[0]):])
		if v.Body == "" {
			apiError(w, 400, "Write a message after @admin.")
			return
		}
	}
	recipient, e := a.recipient(v.Recipient)
	if e != nil {
		apiError(w, 400, e.Error())
		return
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		apiError(w, 500, "Could not save the message.")
		return
	}
	defer tx.Rollback()
	var count int64
	if e = tx.QueryRow("SELECT count(*) FROM messages").Scan(&count); e != nil {
		apiError(w, 500, "Could not check chat storage.")
		return
	}
	if count >= 50000 {
		var existing int
		if tx.QueryRow("SELECT 1 FROM messages WHERE user_id=? AND client_token=?", s.UID, v.Token).Scan(&existing) == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		apiError(w, 507, "Chat storage is full. Ask the host to clear older messages.")
		return
	}
	var room string
	if e = tx.QueryRow("SELECT room FROM users WHERE id=?", s.UID).Scan(&room); e != nil {
		apiError(w, 500, "Could not find your room.")
		return
	}
	if recipient == nil && v.RoomHint != nil && *v.RoomHint != room {
		apiError(w, 409, "Your room changed. Refresh before sending this message.")
		return
	}
	storedBody := v.Body
	encrypted := 0
	if a.Store.Vault.encrypting() {
		storedBody = ""
		encrypted = 1
	}
	result, e := tx.Exec("INSERT OR IGNORE INTO messages(user_id,recipient_id,body,created_at,client_token,body_encrypted,room) VALUES(?,?,?,?,?,?,?)", s.UID, recipient, storedBody, timestamp(), v.Token, encrypted, room)
	if e != nil {
		apiError(w, 500, "Could not save the message.")
		return
	}
	if affected, _ := result.RowsAffected(); affected > 0 && encrypted == 1 {
		id, _ := result.LastInsertId()
		body, err := a.Store.Vault.encodeMessage(id, v.Body)
		if err != nil {
			apiError(w, 500, "Could not encrypt the message.")
			return
		}
		if _, err = tx.Exec("UPDATE messages SET body=? WHERE id=?", body, id); err != nil {
			apiError(w, 500, "Could not save the encrypted message.")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "Could not save the message.")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func pointer(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}
func (a *API) chatData(w http.ResponseWriter, r *http.Request, s APISession) {
	section := r.URL.Query().Get("section")
	if section != "" && section != "chat" && section != "files" && section != "people" {
		apiError(w, 400, "Unknown chat section.")
		return
	}
	limit := 30
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 30 {
			apiError(w, 400, "Message page size must be 1 to 30.")
			return
		}
		limit = value
	}
	room, e := a.requireRoom(s.UID)
	if e != nil {
		apiError(w, 401, e.Error())
		return
	}
	roomNames, e := a.rooms()
	if e != nil {
		apiError(w, 500, "Could not load rooms.")
		return
	}
	roomChanged := r.URL.Query().Get("room_hint") != "" && r.URL.Query().Get("room_hint") != room
	messages := []Message{}
	files := []FileInfo{}
	more := false
	if section == "" || section == "chat" {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		after, _ := strconv.ParseInt(r.URL.Query().Get("message_after"), 10, 64)
		if roomChanged {
			before = 0
			after = 0
		}
		query := "SELECT m.id,m.body,m.created_at,m.recipient_id,u.display_name,m.body_encrypted,m.user_id,m.room,coalesce(target.display_name,'') FROM messages m JOIN users u ON u.id=m.user_id LEFT JOIN users target ON target.id=m.recipient_id WHERE ((m.recipient_id IS NULL AND m.room=?) OR (m.recipient_id IS NOT NULL AND (m.user_id=? OR m.recipient_id=?)))"
		args := []any{room, s.UID, s.UID}
		if before > 0 {
			query += " AND m.id<?"
			args = append(args, before)
		} else if after > 0 {
			query += " AND m.id>?"
			args = append(args, after)
		}
		query += " ORDER BY m.id DESC LIMIT ?"
		args = append(args, limit+1)
		rows, e := a.Store.DB.Query(query, args...)
		if e != nil {
			apiError(w, 500, "Could not load chat.")
			return
		}
		for rows.Next() {
			var m Message
			var recipient sql.NullInt64
			if e = rows.Scan(&m.ID, &m.Body, &m.Created, &recipient, &m.Display, &m.Encrypted, &m.UserID, &m.Room, &m.RecipientDisplay); e != nil {
				break
			}
			m.Recipient = pointer(recipient)
			m.Body, e = a.Store.Vault.decodeMessage(m.ID, m.Body, m.Encrypted)
			if e != nil {
				rows.Close()
				apiError(w, 500, "Could not authenticate encrypted history.")
				return
			}
			messages = append(messages, m)
		}
		err := rows.Err()
		rows.Close()
		if e != nil || err != nil {
			apiError(w, 500, "Could not load chat.")
			return
		}
		more = len(messages) > limit
		if more {
			messages = messages[:limit]
		}
		for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
			messages[i], messages[j] = messages[j], messages[i]
		}
	}
	if section == "" || section == "files" {
		rows, e := a.Store.DB.Query("SELECT f.id,f.original_name,f.size,f.created_at,f.recipient_id,u.display_name FROM files f JOIN users u ON u.id=f.user_id WHERE (f.recipient_id IS NULL AND f.room=?) OR (f.recipient_id IS NOT NULL AND (f.user_id=? OR f.recipient_id=?)) ORDER BY f.id DESC LIMIT 50", room, s.UID, s.UID)
		if e != nil {
			apiError(w, 500, "Could not load files.")
			return
		}
		for rows.Next() {
			var f FileInfo
			var recipient sql.NullInt64
			if e = rows.Scan(&f.ID, &f.Name, &f.Size, &f.Created, &recipient, &f.Display); e != nil {
				break
			}
			f.Recipient = pointer(recipient)
			files = append(files, f)
		}
		err := rows.Err()
		rows.Close()
		if e != nil || err != nil {
			apiError(w, 500, "Could not load files.")
			return
		}
	}
	users := []User{}
	if r.URL.Query().Get("include_users") != "0" {
		var err error
		users, err = a.users(false, s.UID)
		if err != nil {
			apiError(w, 500, "Could not load people.")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"messages": messages, "files": files, "users": users, "more": more, "upload_limit": a.currentUploadLimit() / (1 << 20), "room": room, "rooms": roomNames, "room_changed": roomChanged})
}
func (a *API) users(all bool, except int64) ([]User, error) {
	allFlag := 0
	if all {
		allFlag = 1
	}
	q := "SELECT id,username,display_name,status,created_at,room FROM users WHERE id!=? AND (?=0 OR username!=?)"
	if !all {
		q += " AND status='approved'"
	}
	q += " ORDER BY display_name COLLATE NOCASE LIMIT 500"
	rows, e := a.Store.DB.Query(q, except, allFlag, adminUsername)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if e = rows.Scan(&u.ID, &u.Username, &u.Display, &u.Status, &u.Created, &u.Room); e != nil {
			return nil, e
		}
		if u.Username == adminUsername {
			u.Username = "admin"
			u.Display = "Host"
		}
		out = append(out, u)
	}
	err := rows.Err()
	rows.Close()
	a.decorate(out, all)
	return out, err
}
func (a *API) search(w http.ResponseWriter, r *http.Request, s APISession) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) < 2 || utf8.RuneCountInString(q) > 80 {
		apiError(w, 400, "Search using 2 to 80 characters.")
		return
	}
	room, e := a.requireRoom(s.UID)
	if e != nil {
		apiError(w, 401, e.Error())
		return
	}
	encryptedMode := 0
	limit := 100
	if a.Store.Vault.encrypting() {
		encryptedMode = 1
		limit = 50000
	}
	rows, e := a.Store.DB.Query("SELECT m.id,m.body,m.created_at,m.recipient_id,u.display_name,m.body_encrypted,m.user_id,m.room FROM messages m JOIN users u ON u.id=m.user_id WHERE ((m.recipient_id IS NULL AND m.room=?) OR (m.recipient_id IS NOT NULL AND (m.user_id=? OR m.recipient_id=?))) AND (?=1 OR instr(lower(m.body),lower(?))>0) ORDER BY m.id DESC LIMIT ?", room, s.UID, s.UID, encryptedMode, q, limit)
	if e != nil {
		apiError(w, 500, "Search failed.")
		return
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var n sql.NullInt64
		if e = rows.Scan(&m.ID, &m.Body, &m.Created, &n, &m.Display, &m.Encrypted, &m.UserID, &m.Room); e != nil {
			apiError(w, 500, "Search failed.")
			return
		}
		m.Recipient = pointer(n)
		m.Body, e = a.Store.Vault.decodeMessage(m.ID, m.Body, m.Encrypted)
		if e != nil {
			apiError(w, 500, "Could not authenticate encrypted history.")
			return
		}
		if !strings.Contains(strings.ToLower(m.Body), strings.ToLower(q)) {
			continue
		}
		if len(out) >= 100 {
			break
		}
		out = append(out, m)
	}
	writeJSON(w, 200, map[string]any{"results": out})
}
func (a *API) currentUploadLimit() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.LimitUntil.IsZero() && time.Now().After(a.LimitUntil) {
		a.UploadLimit = maxFileBytes
		a.LimitUntil = time.Time{}
	}
	return a.UploadLimit
}
func (a *API) upload(w http.ResponseWriter, r *http.Request, s APISession) {
	select {
	case a.uploads <- struct{}{}:
		defer func() { <-a.uploads }()
	default:
		apiError(w, 429, "Another upload is in progress. Try again shortly.")
		return
	}
	limit := a.currentUploadLimit()
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	reader, e := r.MultipartReader()
	if e != nil {
		apiError(w, 400, "Choose a file to upload.")
		return
	}
	stored := randomToken(24)
	path, e := safeUploadPath(a.Store.Data, stored)
	if e != nil {
		apiError(w, 500, "Upload failed.")
		return
	}
	encrypted := 0
	if a.Store.Vault.encrypting() {
		encrypted = 1
		path += ".nc3e"
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(path)
		}
	}()
	var size int64
	name, recipient := "", ""
	var roomHint *string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			apiError(w, 413, "The upload was interrupted or too large.")
			return
		}
		if part.FormName() == "room_hint" {
			b, err := io.ReadAll(io.LimitReader(part, 258))
			if err != nil || len(b) > 256 {
				apiError(w, 400, "Invalid room hint.")
				return
			}
			hint := string(b)
			roomHint = &hint
		} else if part.FormName() == "recipient_id" {
			b, err := io.ReadAll(io.LimitReader(part, 65))
			if err != nil || len(b) > 64 {
				apiError(w, 400, "Invalid recipient.")
				return
			}
			recipient = string(b)
		} else if part.FormName() == "file" && part.FileName() != "" {
			if name != "" {
				apiError(w, 400, "Upload one file at a time.")
				return
			}
			name = filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
			name = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
					return -1
				}
				return r
			}, name)
			if name == "." || name == "" || len(name) > 180 {
				apiError(w, 400, "Use a shorter filename.")
				return
			}
			destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				apiError(w, 500, "Could not create the upload.")
				return
			}
			if encrypted == 1 {
				size, err = a.Store.Vault.encryptFile(destination, part, stored, limit)
			} else {
				size, err = io.Copy(destination, io.LimitReader(part, limit+1))
			}
			closeErr := destination.Close()
			if err != nil || closeErr != nil || size > limit {
				apiError(w, 413, "The file exceeds the current upload limit.")
				return
			}
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 4097))
		}
		part.Close()
	}
	if name == "" {
		apiError(w, 400, "Choose a file.")
		return
	}
	target, e := a.recipient(recipient)
	if e != nil {
		apiError(w, 400, e.Error())
		return
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		apiError(w, 500, "Upload failed.")
		return
	}
	defer tx.Rollback()
	var used int64
	var count int
	if e = tx.QueryRow("SELECT count(*) FROM files").Scan(&count); e != nil || count >= 1000 {
		apiError(w, 507, "The server has reached its file-count limit. Ask the host to remove old files.")
		return
	}
	if e = tx.QueryRow("SELECT COALESCE(sum(size),0) FROM files").Scan(&used); e != nil || used+size > maxStoredBytes {
		apiError(w, 413, "The server's 256 MB file storage is full. Ask the host to remove files.")
		return
	}
	var room string
	if e = tx.QueryRow("SELECT room FROM users WHERE id=?", s.UID).Scan(&room); e != nil {
		apiError(w, 500, "Could not find your room.")
		return
	}
	if target == nil && roomHint != nil && *roomHint != room {
		apiError(w, 409, "Your room changed. Refresh before sharing this file.")
		return
	}
	if _, e = tx.Exec("INSERT INTO files(user_id,recipient_id,stored_name,original_name,size,created_at,encrypted,room) VALUES(?,?,?,?,?,?,?,?)", s.UID, target, stored, name, size, timestamp(), encrypted, room); e != nil {
		apiError(w, 500, "Upload failed.")
		return
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "Upload failed.")
		return
	}
	keep = true
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *API) download(w http.ResponseWriter, r *http.Request, s APISession) {
	id, e := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/download/"), 10, 64)
	if e != nil || id <= 0 {
		apiError(w, 404, "File not found.")
		return
	}
	var stored, name string
	var encrypted int
	var size int64
	if a.Store.DB.QueryRow("SELECT stored_name,original_name,encrypted,size FROM files WHERE id=? AND ((recipient_id IS NULL AND room=(SELECT room FROM users WHERE id=?)) OR (recipient_id IS NOT NULL AND (user_id=? OR recipient_id=?)))", id, s.UID, s.UID, s.UID).Scan(&stored, &name, &encrypted, &size) != nil {
		apiError(w, 404, "File not found.")
		return
	}
	path, e := safeUploadPath(a.Store.Data, stored)
	if e != nil {
		apiError(w, 404, "File not found.")
		return
	}
	if encrypted == 1 {
		path += ".nc3e"
	}
	f, e := os.Open(path)
	if e != nil {
		apiError(w, 404, "File not found.")
		return
	}
	defer f.Close()
	if encrypted == 1 {
		if verified, err := a.Store.Vault.decryptFile(io.Discard, f, stored); err != nil || verified != size {
			apiError(w, 500, "This encrypted file failed its integrity check.")
			return
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			apiError(w, 500, "Could not read this file.")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = a.Store.Vault.decryptFile(w, f, stored)
		return
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		apiError(w, 404, "File not found.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, info.ModTime(), f)
}
func (a *API) adminData(w http.ResponseWriter, r *http.Request) {
	users, e := a.users(true, 0)
	if e != nil {
		apiError(w, 500, "Could not load accounts.")
		return
	}
	pending := []User{}
	for _, u := range users {
		if u.Status == "pending" {
			pending = append(pending, u)
		}
	}
	var messages, files int64
	_ = a.Store.DB.QueryRow("SELECT count(*) FROM messages").Scan(&messages)
	_ = a.Store.DB.QueryRow("SELECT count(*) FROM files").Scan(&files)
	rows, e := a.Store.DB.Query("SELECT id,original_name,size,created_at FROM files ORDER BY id DESC LIMIT 100")
	if e != nil {
		apiError(w, 500, "Could not load files.")
		return
	}
	fileList := []FileInfo{}
	for rows.Next() {
		var f FileInfo
		if rows.Scan(&f.ID, &f.Name, &f.Size, &f.Created) == nil {
			fileList = append(fileList, f)
		}
	}
	rows.Close()
	messageList := []Message{}
	rows, e = a.Store.DB.Query("SELECT m.id,m.body,m.created_at,m.recipient_id,u.display_name,m.body_encrypted FROM messages m JOIN users u ON u.id=m.user_id ORDER BY m.id DESC LIMIT 100")
	if e != nil {
		apiError(w, 500, "Could not load messages.")
		return
	}
	for rows.Next() {
		var m Message
		var recipient sql.NullInt64
		if e = rows.Scan(&m.ID, &m.Body, &m.Created, &recipient, &m.Display, &m.Encrypted); e != nil {
			rows.Close()
			apiError(w, 500, "Could not load messages.")
			return
		}
		m.Recipient = pointer(recipient)
		m.Body, e = a.Store.Vault.decodeMessage(m.ID, m.Body, m.Encrypted)
		if e != nil {
			rows.Close()
			apiError(w, 500, "Could not authenticate encrypted history.")
			return
		}
		messageList = append(messageList, m)
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"pending": pending, "users": users, "files": fileList, "messages": messageList, "counts": map[string]int64{"users": int64(len(users)), "messages": messages, "files": files}, "upload_limit": a.currentUploadLimit() / (1 << 20)})
}
func (a *API) adminAction(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	if !decodeJSON(w, r, &v) {
		return
	}
	action, _ := v["action"].(string)
	if action == "upload-limit" {
		mb, ok1 := intValue(v["limit_mb"])
		minutes, ok2 := intValue(v["minutes"])
		if !ok1 || !ok2 || mb > 100 || minutes > 1440 {
			apiError(w, 400, "Choose 1 to 100 MB and 1 to 1,440 minutes.")
			return
		}
		a.mu.Lock()
		a.UploadLimit = mb << 20
		a.LimitUntil = time.Now().Add(time.Duration(minutes) * time.Minute)
		a.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if action == "upload-limit-reset" {
		a.mu.Lock()
		a.UploadLimit = maxFileBytes
		a.LimitUntil = time.Time{}
		a.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if action == "rotate-join-code" {
		if e := a.Store.rotateJoin(); e != nil {
			apiError(w, 500, "Could not change the join code.")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if action == "history" {
		days, ok := intValue(v["days"])
		if !ok || days > 3650 {
			apiError(w, 400, "Choose 1 to 3,650 days.")
			return
		}
		if _, e := a.Store.DB.Exec("DELETE FROM messages WHERE julianday(created_at)<julianday('now',?)", fmt.Sprintf("-%d days", days)); e != nil {
			apiError(w, 500, "Could not remove old messages.")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	id, ok := intValue(v["id"])
	if !ok {
		apiError(w, 400, "Choose a valid item.")
		return
	}
	var e error
	switch action {
	case "approve", "enable", "disable":
		status := "approved"
		if action == "disable" {
			status = "disabled"
		}
		_, e = a.Store.DB.Exec("UPDATE users SET status=?,auth_version=auth_version+1 WHERE id=? AND username NOT IN (?,?)", status, id, adminUsername, "admin")
	case "delete-message":
		_, e = a.Store.DB.Exec("DELETE FROM messages WHERE id=?", id)
	case "delete-file":
		var stored string
		if a.Store.DB.QueryRow("SELECT stored_name FROM files WHERE id=?", id).Scan(&stored) == nil {
			path, err := safeUploadPath(a.Store.Data, stored)
			if err != nil {
				apiError(w, 400, "Invalid stored filename.")
				return
			}
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				apiError(w, 500, "Could not remove that file.")
				return
			}
			if err = os.Remove(path + ".nc3e"); err != nil && !os.IsNotExist(err) {
				apiError(w, 500, "Could not remove that file.")
				return
			}
		}
		_, e = a.Store.DB.Exec("DELETE FROM files WHERE id=?", id)
	case "delete-account":
		e = a.deleteAccount(id)
	default:
		apiError(w, 400, "That action is not available.")
		return
	}
	if e != nil {
		apiError(w, 500, "Could not finish that action.")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *API) deleteAccount(id int64) error {
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var name string
	if e = tx.QueryRow("SELECT username FROM users WHERE id=?", id).Scan(&name); e != nil {
		return e
	}
	if name == adminUsername || name == "admin" {
		return errors.New("cannot delete host")
	}
	rows, e := tx.Query("SELECT stored_name FROM files WHERE user_id=? OR recipient_id=?", id, id)
	if e != nil {
		return e
	}
	names := []string{}
	for rows.Next() {
		var stored string
		if e = rows.Scan(&stored); e != nil {
			rows.Close()
			return e
		}
		names = append(names, stored)
	}
	rows.Close()
	for _, q := range []string{"DELETE FROM messages WHERE user_id=? OR recipient_id=?", "DELETE FROM files WHERE user_id=? OR recipient_id=?"} {
		if _, e = tx.Exec(q, id, id); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("DELETE FROM users WHERE id=?", id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	for _, name := range names {
		if p, err := safeUploadPath(a.Store.Data, name); err == nil {
			_ = os.Remove(p)
			_ = os.Remove(p + ".nc3e")
		}
	}
	return nil
}
