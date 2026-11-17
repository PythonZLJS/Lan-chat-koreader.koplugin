package main

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const adminUsername = "server_admin_system"

type Store struct {
	Vault               *Vault
	DB                  *sql.DB
	Root, Data, Backups string
	mu                  sync.RWMutex
	AdminHash, JoinCode string
	AdminVersion        int64
	Cert                tls.Certificate
	Fingerprint         string
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".new-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}
func readText(path string) string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
func openStore(root string) (*Store, error) {
	absolute, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	data, e := ensureDataRoot(absolute)
	if e != nil {
		return nil, e
	}
	if e = migrateLegacyStorage(absolute, data); e != nil {
		return nil, e
	}
	s := &Store{Root: absolute, Data: data}
	if e = os.MkdirAll(filepath.Join(s.Data, "uploads"), 0700); e != nil {
		return nil, e
	}
	if s.Backups, e = backupDirectory(absolute); e != nil {
		return nil, e
	}
	// Back up the legacy database consistently before any schema migration.
	dbPath := filepath.Join(s.Data, "chat.sqlite3")
	db, e := sql.Open("sqlite", dbPath)
	if e != nil {
		return nil, e
	}
	s.DB = db
	db.SetMaxOpenConns(1)
	failed := true
	defer func() {
		if failed {
			db.Close()
		}
	}()
	if _, e = db.Exec("PRAGMA busy_timeout=10000; PRAGMA foreign_keys=ON; PRAGMA secure_delete=ON;"); e != nil {
		return nil, e
	}
	if _, e = os.Stat(filepath.Join(s.Data, "native-v3-migration.done")); os.IsNotExist(e) {
		var count int
		if e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users'").Scan(&count); e != nil {
			return nil, e
		}
		if count > 0 {
			backup := filepath.Join(s.Backups, "chat-before-v3.sqlite3")
			if _, e = os.Stat(backup); os.IsNotExist(e) {
				if _, e = db.Exec("VACUUM INTO '" + strings.ReplaceAll(backup, "'", "''") + "'"); e != nil {
					return nil, fmt.Errorf("backup before migration: %w", e)
				}
			}
		}
	}
	schema := `CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY,username TEXT UNIQUE NOT NULL,display_name TEXT NOT NULL,password_hash TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',created_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),recipient_id INTEGER REFERENCES users(id),body TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS files(id INTEGER PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),recipient_id INTEGER REFERENCES users(id),stored_name TEXT NOT NULL UNIQUE,original_name TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_messages_recipient_id ON messages(recipient_id,id);
 CREATE INDEX IF NOT EXISTS idx_files_recipient_id ON files(recipient_id,id);`
	if _, e = db.Exec(schema); e != nil {
		return nil, e
	}
	if e = ensureColumn(db, "messages", "body_encrypted", "INTEGER NOT NULL DEFAULT 0"); e != nil {
		return nil, e
	}
	if e = ensureColumn(db, "files", "encrypted", "INTEGER NOT NULL DEFAULT 0"); e != nil {
		return nil, e
	}
	s.Vault, e = loadVault(s.Data)
	if e != nil {
		return nil, e
	}
	if s.Vault.config == nil {
		var protected int
		if e = db.QueryRow("SELECT (SELECT count(*) FROM messages WHERE body_encrypted!=0)+(SELECT count(*) FROM files WHERE encrypted!=0)").Scan(&protected); e != nil {
			return nil, e
		}
		if protected > 0 {
			return nil, errors.New("local encryption settings are missing; restore local-encryption.json from your data backup")
		}
	}
	rows, e := db.Query("PRAGMA table_info(messages)")
	if e != nil {
		return nil, e
	}
	hasToken := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if e = rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); e != nil {
			rows.Close()
			return nil, e
		}
		if name == "client_token" {
			hasToken = true
		}
	}
	rows.Close()
	if !hasToken {
		if _, e = db.Exec("ALTER TABLE messages ADD COLUMN client_token TEXT"); e != nil {
			return nil, e
		}
	}
	if _, e = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_message_token ON messages(user_id,client_token) WHERE client_token IS NOT NULL"); e != nil {
		return nil, e
	}
	if e = s.migrateCollaboration(); e != nil {
		return nil, e
	}
	if _, e = db.Exec("INSERT OR IGNORE INTO users(username,display_name,password_hash,status,created_at) VALUES(?,?,?,'approved',?)", adminUsername, "Host", "disabled-system-login", timestamp()); e != nil {
		return nil, e
	}
	// Host ownership is granted by ownerRemote, not a host password. Ignore
	// and retire password material created by previous releases.
	s.AdminHash = ""
	_ = os.Remove(filepath.Join(s.Data, "initial-admin-password.txt"))
	_ = os.Remove(filepath.Join(s.Data, "admin-password.hash"))
	s.JoinCode = readText(filepath.Join(s.Data, "join-code.txt"))
	if s.JoinCode == "" {
		s.JoinCode = strings.ToUpper(randomToken(9))
		if e = atomicWrite(filepath.Join(s.Data, "join-code.txt"), []byte(s.JoinCode), 0600); e != nil {
			return nil, e
		}
	}
	_, _ = fmt.Sscan(readText(filepath.Join(s.Data, "admin-auth-version.txt")), &s.AdminVersion)
	if e = s.loadCertificate(); e != nil {
		return nil, e
	}
	if e = atomicWrite(filepath.Join(s.Data, "native-v3-migration.done"), []byte("3\n"), 0600); e != nil {
		return nil, e
	}
	failed = false
	return s, nil
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }
func (s *Store) adminState() (string, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AdminHash, s.AdminVersion
}
func (s *Store) changeAdmin(current, next string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !verifyPassword(s.AdminHash, current) {
		return errors.New("Current password is incorrect.")
	}
	hash, e := hashPassword(next)
	if e != nil {
		return e
	}
	version := s.AdminVersion + 1
	// Revoking old sessions first is safe even if the subsequent password write fails.
	if e = atomicWrite(filepath.Join(s.Data, "admin-auth-version.txt"), []byte(fmt.Sprint(version)), 0600); e != nil {
		return e
	}
	s.AdminVersion = version
	if e = atomicWrite(filepath.Join(s.Data, "admin-password.hash"), []byte(hash), 0600); e != nil {
		return e
	}
	s.AdminHash = hash
	_ = os.Remove(filepath.Join(s.Data, "initial-admin-password.txt"))
	return nil
}
func (s *Store) joinCode() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.JoinCode }
func (s *Store) rotateJoin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := strings.ToUpper(randomToken(9))
	if e := atomicWrite(filepath.Join(s.Data, "join-code.txt"), []byte(code), 0600); e != nil {
		return e
	}
	s.JoinCode = code
	return nil
}
func safeUploadPath(data, name string) (string, error) {
	if name == "" || len(name) > 128 || filepath.Base(name) != name || strings.ContainsAny(name, "/\\:") || name == "." || name == ".." {
		return "", errors.New("invalid upload path")
	}
	p := filepath.Join(data, "uploads", name)
	if info, e := os.Lstat(p); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("upload symlinks are not permitted")
	}
	return p, nil
}
