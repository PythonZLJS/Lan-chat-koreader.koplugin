package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Presence struct {
	Seen    time.Time
	IP      string
	Latency int64
}
type IPBan struct {
	IP     string `json:"ip"`
	UserID int64  `json:"user_id"`
}
type HostData struct {
	Users   []User   `json:"users"`
	Pending []User   `json:"pending"`
	Rooms   []string `json:"rooms"`
	Bans    []IPBan  `json:"bans"`
}

func (s *Store) migrateCollaboration() error {
	rows, err := s.DB.Query("PRAGMA table_info(messages)")
	if err != nil {
		return err
	}
	hasRoom := false
	for rows.Next() {
		var id, nn, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&id, &name, &kind, &nn, &value, &pk); err != nil {
			rows.Close()
			return err
		}
		hasRoom = hasRoom || name == "room"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasRoom {
		var count int
		if err = s.DB.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil {
			return err
		}
		backup := filepath.Join(s.Backups, "chat-before-v6.sqlite3")
		if count > 0 {
			if _, e := os.Stat(backup); os.IsNotExist(e) {
				_, err = s.DB.Exec("VACUUM INTO '" + strings.ReplaceAll(backup, "'", "''") + "'")
				if err != nil {
					return err
				}
			} else if e != nil {
				return e
			}
		}
	}
	for _, column := range []struct{ table, name, definition string }{
		{"users", "room", "TEXT NOT NULL DEFAULT 'Lobby'"}, {"users", "auth_version", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "room", "TEXT NOT NULL DEFAULT 'Lobby'"}, {"files", "room", "TEXT NOT NULL DEFAULT 'Lobby'"},
	} {
		if err = ensureColumn(s.DB, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	_, err = s.DB.Exec(`CREATE TABLE IF NOT EXISTS rooms(name TEXT COLLATE NOCASE PRIMARY KEY);
INSERT OR IGNORE INTO rooms(name) VALUES('Lobby');
CREATE TABLE IF NOT EXISTS banned_ips(ip TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS idx_messages_room ON messages(room,id);
CREATE INDEX IF NOT EXISTS idx_files_room ON files(room,id);`)
	if err != nil {
		return err
	}
	return s.migrateIPBans()
}

// Preserve overlapping account bans on the same Wi-Fi IP independently.
func (s *Store) migrateIPBans() error {
	rows, err := s.DB.Query("PRAGMA table_info(banned_ips)")
	if err != nil {
		return err
	}
	composite := false
	for rows.Next() {
		var id, nn, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&id, &name, &kind, &nn, &value, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "user_id" && pk > 0 {
			composite = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || composite {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE banned_ips_v71(ip TEXT NOT NULL,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,PRIMARY KEY(ip,user_id));
 INSERT INTO banned_ips_v71 SELECT ip,user_id FROM banned_ips;
 DROP TABLE banned_ips;
 ALTER TABLE banned_ips_v71 RENAME TO banned_ips;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (a *API) currentRoom(uid int64) (string, error) {
	var room string
	err := a.Store.DB.QueryRow("SELECT room FROM users WHERE id=?", uid).Scan(&room)
	return room, err
}
func (a *API) rooms() ([]string, error) {
	rows, err := a.Store.DB.Query("SELECT name FROM rooms ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms := []string{}
	for rows.Next() {
		var room string
		if err = rows.Scan(&room); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}
func (a *API) ipBanned(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.IsLoopback() {
		return false
	}
	var found int
	return a.Store.DB.QueryRow("SELECT 1 FROM banned_ips WHERE ip=?", parsed.String()).Scan(&found) == nil
}
func (a *API) touch(s APISession, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.presence == nil {
		a.presence = make(map[int64]Presence)
	}
	now := time.Now()
	if len(a.presence) >= 1024 {
		for id, p := range a.presence {
			if now.Sub(p.Seen) > 3*time.Minute {
				delete(a.presence, id)
			}
		}
	}
	if len(a.presence) >= 1024 {
		return
	}
	p := a.presence[s.UID]
	p.Seen = now
	p.IP = clientIP(r)
	if ms, err := strconv.ParseInt(r.URL.Query().Get("rtt_ms"), 10, 64); err == nil && ms >= 0 && ms <= 120000 {
		p.Latency = ms
	}
	a.presence[s.UID] = p
}
func (a *API) decorate(users []User, admin bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range users {
		if p, ok := a.presence[users[i].ID]; ok {
			users[i].Online = time.Since(p.Seen) < 90*time.Second
			users[i].LatencyMS = p.Latency
			if admin {
				users[i].IP = p.IP
			}
		}
	}
}
func (a *API) hostData(w http.ResponseWriter, r *http.Request) {
	users, err := a.users(true, 0)
	if err != nil {
		apiError(w, 500, "Could not load accounts.")
		return
	}
	rooms, err := a.rooms()
	if err != nil {
		apiError(w, 500, "Could not load rooms.")
		return
	}
	data := HostData{Users: users, Rooms: rooms, Pending: []User{}, Bans: []IPBan{}}
	for _, u := range users {
		if u.Status == "pending" {
			data.Pending = append(data.Pending, u)
		}
	}
	rows, err := a.Store.DB.Query("SELECT ip,user_id FROM banned_ips ORDER BY ip")
	if err != nil {
		apiError(w, 500, "Could not load IP bans.")
		return
	}
	for rows.Next() {
		var ban IPBan
		if err = rows.Scan(&ban.IP, &ban.UserID); err != nil {
			break
		}
		data.Bans = append(data.Bans, ban)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil || readErr != nil {
		apiError(w, 500, "Could not load IP bans.")
		return
	}
	writeJSON(w, 200, data)
}
func validRoomName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 || name != strings.TrimSpace(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}
func (a *API) collaborationAction(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Action string `json:"action"`
		ID     int64  `json:"id"`
		Room   string `json:"room"`
	}
	if !decodeJSON(w, r, &v) {
		return
	}
	var err error
	switch v.Action {
	case "create-room":
		if !validRoomName(v.Room) {
			apiError(w, 400, "Use a room name with 1 to 64 printable characters.")
			return
		}
		var count int
		_ = a.Store.DB.QueryRow("SELECT count(*) FROM rooms").Scan(&count)
		if count >= 100 {
			apiError(w, 400, "The host can keep up to 100 rooms.")
			return
		}
		_, err = a.Store.DB.Exec("INSERT INTO rooms(name) VALUES(?)", v.Room)
	case "delete-room":
		var tx *sql.Tx
		tx, err = a.Store.DB.Begin()
		if err != nil {
			break
		}
		defer tx.Rollback()
		var canonical string
		if err = tx.QueryRow("SELECT name FROM rooms WHERE name=?", v.Room).Scan(&canonical); err != nil {
			apiError(w, 400, "Choose an existing room.")
			return
		}
		v.Room = canonical
		if strings.EqualFold(v.Room, "Lobby") {
			apiError(w, 400, "Lobby cannot be deleted.")
			return
		}
		// Keep history under its original room. Recreating a deleted name does
		// not expose previous public room content to newly assigned users.
		_, err = tx.Exec("UPDATE users SET room='Lobby' WHERE room=?", v.Room)
		if err == nil {
			_, err = tx.Exec("UPDATE messages SET room=? WHERE room=? AND recipient_id IS NULL", "archived-"+randomToken(16), v.Room)
		}
		if err == nil {
			_, err = tx.Exec("UPDATE files SET room=? WHERE room=? AND recipient_id IS NULL", "archived-"+randomToken(16), v.Room)
		}
		if err == nil {
			_, err = tx.Exec("DELETE FROM rooms WHERE name=?", v.Room)
		}
		if err == nil {
			err = tx.Commit()
		}
	case "move-user":
		var room string
		if v.ID < 1 || a.Store.DB.QueryRow("SELECT name FROM rooms WHERE name=?", v.Room).Scan(&room) != nil {
			apiError(w, 400, "Choose an existing room and account.")
			return
		}
		_, err = a.Store.DB.Exec("UPDATE users SET room=? WHERE id=? AND status='approved'", room, v.ID)
	case "kick", "ban", "unban":
		var username string
		if v.ID < 1 || a.Store.DB.QueryRow("SELECT username FROM users WHERE id=?", v.ID).Scan(&username) != nil || username == adminUsername || username == "admin" {
			apiError(w, 400, "Choose a regular account.")
			return
		}
		var tx *sql.Tx
		tx, err = a.Store.DB.Begin()
		if err != nil {
			break
		}
		defer tx.Rollback()
		status := "approved"
		if v.Action == "ban" {
			status = "banned"
		}
		if v.Action == "kick" {
			_, err = tx.Exec("UPDATE users SET auth_version=auth_version+1 WHERE id=?", v.ID)
		} else {
			_, err = tx.Exec("UPDATE users SET status=?,auth_version=auth_version+1 WHERE id=?", status, v.ID)
		}
		if err == nil && v.Action == "ban" {
			a.mu.Lock()
			ip := net.ParseIP(a.presence[v.ID].IP)
			a.mu.Unlock()
			if ip != nil && ip.IsPrivate() && !ip.IsLoopback() {
				_, err = tx.Exec("INSERT OR IGNORE INTO banned_ips(ip,user_id) VALUES(?,?)", ip.String(), v.ID)
			}
		}
		if err == nil && v.Action == "unban" {
			_, err = tx.Exec("DELETE FROM banned_ips WHERE user_id=?", v.ID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err == nil {
			a.mu.Lock()
			delete(a.presence, v.ID)
			a.mu.Unlock()
		}
	default:
		apiError(w, 400, "That host action is not available.")
		return
	}
	if err != nil {
		apiError(w, 400, "Could not finish that action. The room may already exist.")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *API) adminTarget() (int64, error) {
	var id int64
	err := a.Store.DB.QueryRow("SELECT id FROM users WHERE username=?", adminUsername).Scan(&id)
	return id, err
}
func (a *API) inbox(w http.ResponseWriter, r *http.Request) {
	id, err := a.adminTarget()
	if err != nil {
		apiError(w, 500, "Host account missing.")
		return
	}
	rows, err := a.Store.DB.Query("SELECT m.id,m.body,m.created_at,m.recipient_id,u.display_name,m.body_encrypted,m.user_id,m.room FROM messages m JOIN users u ON u.id=m.user_id WHERE m.recipient_id=? ORDER BY m.id DESC LIMIT 50", id)
	if err != nil {
		apiError(w, 500, "Could not load host inbox.")
		return
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var m Message
		var recipient sql.NullInt64
		if err = rows.Scan(&m.ID, &m.Body, &m.Created, &recipient, &m.Display, &m.Encrypted, &m.UserID, &m.Room); err != nil {
			break
		}
		m.Recipient = pointer(recipient)
		m.Body, err = a.Store.Vault.decodeMessage(m.ID, m.Body, m.Encrypted)
		if err != nil {
			break
		}
		messages = append(messages, m)
	}
	if err != nil || rows.Err() != nil {
		apiError(w, 500, "Could not authenticate host inbox.")
		return
	}
	writeJSON(w, 200, map[string]any{"messages": messages})
}

func (a *App) forgetHost(origin string) error {
	a.savedMu.Lock()
	defer a.savedMu.Unlock()
	hosts := []SavedHost{}
	for _, h := range a.saved {
		if h.Origin != origin {
			hosts = append(hosts, h)
		}
	}
	data, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(a.Store.Data, "trusted-hosts.json"), data, 0600); err != nil {
		return err
	}
	a.saved = hosts
	return nil
}
func (a *API) requireRoom(uid int64) (string, error) {
	room, err := a.currentRoom(uid)
	if err != nil {
		return "", errors.New("The account is no longer available.")
	}
	return room, nil
}
