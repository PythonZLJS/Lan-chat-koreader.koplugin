package main

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

type vaultConfig struct {
	MAC           string `json:"mac"`
	Version       int    `json:"version"`
	Enabled       bool   `json:"enabled"`
	Phase         string `json:"phase"`
	Salt, Wrapped string
}
type Vault struct {
	mu     sync.RWMutex
	config *vaultConfig
	key    []byte
	path   string
}

func loadVault(data string) (*Vault, error) {
	v := &Vault{path: filepath.Join(data, "local-encryption.json")}
	b, e := os.ReadFile(v.path)
	if os.IsNotExist(e) {
		return v, nil
	}
	if e != nil {
		return nil, e
	}
	var config vaultConfig
	if e = json.Unmarshal(b, &config); e != nil || config.Version != 1 || (config.Phase != "ready" && config.Phase != "enabling" && config.Phase != "disabling") {
		return nil, errors.New("saved encryption settings are damaged; restore local-encryption.json from backup")
	}
	v.config = &config
	return v, nil
}
func (v *Vault) status() (enabled, locked bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.config != nil, v.config != nil && (v.key == nil || v.config.Phase != "ready")
}
func (v *Vault) pending() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.config != nil && v.config.Phase != "ready"
}
func (v *Vault) ready() bool      { return v.config == nil || (v.key != nil && v.config.Phase == "ready") }
func (v *Vault) encrypting() bool { return v.config != nil && v.config.Enabled }
func (v *Vault) save() error {
	mac := hmac.New(sha256.New, v.key)
	_, _ = fmt.Fprintf(mac, "nc3/vault-state/%t/%s", v.config.Enabled, v.config.Phase)
	v.config.MAC = base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
	b, e := json.Marshal(v.config)
	if e != nil {
		return e
	}
	return atomicWrite(v.path, b, 0600)
}
func vaultPasswordKey(password string, salt []byte) []byte {
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots }()
	return argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
}
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
func (v *Vault) seal(plain []byte, context string) ([]byte, error) {
	if len(v.key) != 32 {
		return nil, errors.New("unlock saved-data encryption first")
	}
	a, e := chacha20poly1305.NewX(v.key)
	if e != nil {
		return nil, e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	return a.Seal(nonce, nonce, plain, []byte(context)), nil
}
func (v *Vault) open(encoded []byte, context string) ([]byte, error) {
	if len(v.key) != 32 {
		return nil, errors.New("unlock saved-data encryption first")
	}
	a, e := chacha20poly1305.NewX(v.key)
	if e != nil {
		return nil, e
	}
	if len(encoded) < a.NonceSize()+a.Overhead() {
		return nil, errors.New("encrypted content is damaged")
	}
	return a.Open(nil, encoded[:a.NonceSize()], encoded[a.NonceSize():], []byte(context))
}
func (v *Vault) encodeMessage(id int64, text string) (string, error) {
	b, e := v.seal([]byte(text), fmt.Sprintf("nc3/message/%d", id))
	return base64.RawStdEncoding.EncodeToString(b), e
}
func (v *Vault) decodeMessage(id int64, text string, encrypted int) (string, error) {
	if encrypted == 0 {
		return text, nil
	}
	b, e := base64.RawStdEncoding.DecodeString(text)
	if e != nil {
		return "", errors.New("encrypted message is damaged")
	}
	plain, e := v.open(b, fmt.Sprintf("nc3/message/%d", id))
	return string(plain), e
}
func (v *Vault) unwrap(password string) ([]byte, error) {
	if !validPasswordInput(password) {
		return nil, errors.New("invalid encryption password")
	}
	if v.config == nil {
		return nil, errors.New("local encryption is off")
	}
	salt, e := base64.RawStdEncoding.DecodeString(v.config.Salt)
	if e != nil || len(salt) != 16 {
		return nil, errors.New("invalid encryption settings")
	}
	wrapped, e := base64.RawStdEncoding.DecodeString(v.config.Wrapped)
	if e != nil {
		return nil, e
	}
	derived := vaultPasswordKey(password, salt)
	defer wipe(derived)
	a, _ := chacha20poly1305.NewX(derived)
	if len(wrapped) != 24+32+16 {
		return nil, errors.New("invalid encryption key")
	}
	key, e := a.Open(nil, wrapped[:24], wrapped[24:], []byte("nc3/vault-key/v1"))
	if e != nil {
		return nil, errors.New("incorrect local encryption password")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "nc3/vault-state/%t/%s", v.config.Enabled, v.config.Phase)
	if !equal(v.config.MAC, base64.RawStdEncoding.EncodeToString(mac.Sum(nil))) {
		wipe(key)
		return nil, errors.New("saved encryption settings failed their integrity check")
	}
	return key, nil
}
func (s *Store) enableEncryption(password string) error {
	v := s.Vault
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.config != nil {
		return errors.New("unlock or disable the current encryption setting first")
	}
	if !validPassword(password) {
		return errors.New("use a password of 10 to 256 characters")
	}
	key := make([]byte, 32)
	salt := make([]byte, 16)
	nonce := make([]byte, 24)
	if _, e := rand.Read(key); e != nil {
		return e
	}
	if _, e := rand.Read(salt); e != nil {
		return e
	}
	if _, e := rand.Read(nonce); e != nil {
		return e
	}
	derived := vaultPasswordKey(password, salt)
	a, _ := chacha20poly1305.NewX(derived)
	wrapped := a.Seal(nonce, nonce, key, []byte("nc3/vault-key/v1"))
	wipe(derived)
	v.config = &vaultConfig{Version: 1, Enabled: true, Phase: "enabling", Salt: base64.RawStdEncoding.EncodeToString(salt), Wrapped: base64.RawStdEncoding.EncodeToString(wrapped)}
	v.key = key
	if e := v.save(); e != nil {
		wipe(key)
		v.key = nil
		v.config = nil
		return e
	}
	return s.migrateEncryption()
}
func (s *Store) unlockEncryption(password string) error {
	v := s.Vault
	v.mu.Lock()
	defer v.mu.Unlock()
	key, e := v.unwrap(password)
	if e != nil {
		return e
	}
	wipe(v.key)
	v.key = key
	if v.config.Phase != "ready" {
		return s.migrateEncryption()
	}
	return nil
}
func (s *Store) disableEncryption(password string) error {
	v := s.Vault
	v.mu.Lock()
	defer v.mu.Unlock()
	key, e := v.unwrap(password)
	if e != nil {
		return e
	}
	wipe(v.key)
	v.key = key
	v.config.Enabled = false
	v.config.Phase = "disabling"
	if e = v.save(); e != nil {
		return e
	}
	return s.migrateEncryption()
}
func (v *Vault) lock() { v.mu.Lock(); defer v.mu.Unlock(); wipe(v.key); v.key = nil }

const chunkSize = 64 << 10

var encryptedFileMagic = []byte("NC3FILE1")

func frameNonce(prefix []byte, counter uint64) []byte {
	nonce := make([]byte, 24)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[16:], counter)
	return nonce
}
func frameAAD(context string, counter uint64, final bool) []byte {
	return []byte(fmt.Sprintf("nc3/file/%s/%d/%t", context, counter, final))
}
func writeFrame(w io.Writer, a cipher.AEAD, prefix []byte, counter uint64, plain []byte, context string, final bool) error {
	encrypted := a.Seal(nil, frameNonce(prefix, counter), plain, frameAAD(context, counter, final))
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(encrypted)))
	if _, e := w.Write(size[:]); e != nil {
		return e
	}
	_, e := w.Write(encrypted)
	return e
}
func (v *Vault) encryptFile(w io.Writer, r io.Reader, context string, limit int64) (int64, error) {
	if len(v.key) != 32 {
		return 0, errors.New("local encryption is locked")
	}
	a, e := chacha20poly1305.NewX(v.key)
	if e != nil {
		return 0, e
	}
	prefix := make([]byte, 16)
	if _, e = rand.Read(prefix); e != nil {
		return 0, e
	}
	if _, e = w.Write(append(append([]byte{}, encryptedFileMagic...), prefix...)); e != nil {
		return 0, e
	}
	buffer := make([]byte, chunkSize)
	var total int64
	var counter uint64
	for {
		n, err := io.ReadFull(r, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return total, err
		}
		if n > 0 {
			total += int64(n)
			if total > limit {
				return total, errors.New("file exceeds the upload limit")
			}
			if e = writeFrame(w, a, prefix, counter, buffer[:n], context, false); e != nil {
				return total, e
			}
			counter++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}
	return total, writeFrame(w, a, prefix, counter, nil, context, true)
}
func (v *Vault) decryptFile(w io.Writer, r io.Reader, context string) (int64, error) {
	if len(v.key) != 32 {
		return 0, errors.New("local encryption is locked")
	}
	a, e := chacha20poly1305.NewX(v.key)
	if e != nil {
		return 0, e
	}
	header := make([]byte, 24)
	if _, e = io.ReadFull(r, header); e != nil || string(header[:8]) != string(encryptedFileMagic) {
		return 0, errors.New("encrypted file header is damaged")
	}
	prefix := header[8:]
	var counter uint64
	var total int64
	for {
		var size [4]byte
		if _, e = io.ReadFull(r, size[:]); e != nil {
			return total, errors.New("encrypted file is incomplete")
		}
		n := binary.BigEndian.Uint32(size[:])
		if n < 16 || n > chunkSize+16 {
			return total, errors.New("encrypted file frame is invalid")
		}
		encrypted := make([]byte, n)
		if _, e = io.ReadFull(r, encrypted); e != nil {
			return total, e
		}
		final := n == 16
		plain, err := a.Open(nil, frameNonce(prefix, counter), encrypted, frameAAD(context, counter, final))
		if err != nil {
			return total, errors.New("encrypted file authentication failed")
		}
		if final {
			var trailing [1]byte
			n, err := r.Read(trailing[:])
			if n != 0 || err != io.EOF {
				return total, errors.New("unexpected encrypted file data")
			}
			return total, nil
		}
		if _, e = w.Write(plain); e != nil {
			return total, e
		}
		total += int64(len(plain))
		counter++
	}
}
func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, e := db.Query("PRAGMA table_info(" + table + ")")
	if e != nil {
		return e
	}
	exists := false
	for rows.Next() {
		var id, notnull, pk int
		var name, kind string
		var value any
		if e = rows.Scan(&id, &name, &kind, &notnull, &value, &pk); e != nil {
			rows.Close()
			return e
		}
		if name == column {
			exists = true
		}
	}
	rows.Close()
	if exists {
		return nil
	}
	_, e = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return e
}
func (s *Store) migrateEncryption() error {
	v := s.Vault
	target := 0
	if v.config.Enabled {
		target = 1
	}
	if _, e := s.DB.Exec("PRAGMA secure_delete=ON"); e != nil {
		return e
	}
	var lastID int64
	for {
		rows, e := s.DB.Query("SELECT id,body,body_encrypted FROM messages WHERE body_encrypted!=? AND id>? ORDER BY id LIMIT 128", target, lastID)
		if e != nil {
			return e
		}
		type item struct {
			id        int64
			body      string
			encrypted int
		}
		batch := []item{}
		for rows.Next() {
			var m item
			if e = rows.Scan(&m.id, &m.body, &m.encrypted); e != nil {
				rows.Close()
				return e
			}
			batch = append(batch, m)
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		for _, m := range batch {
			body := m.body
			if target == 1 {
				body, e = v.encodeMessage(m.id, body)
			} else {
				body, e = v.decodeMessage(m.id, body, m.encrypted)
			}
			if e == nil {
				_, e = tx.Exec("UPDATE messages SET body=?,body_encrypted=? WHERE id=?", body, target, m.id)
			}
			if e != nil {
				tx.Rollback()
				return e
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		lastID = batch[len(batch)-1].id
	}
	rows, e := s.DB.Query("SELECT id,stored_name,encrypted FROM files")
	if e != nil {
		return e
	}
	type file struct {
		id        int64
		name      string
		encrypted int
	}
	files := []file{}
	for rows.Next() {
		var f file
		if e = rows.Scan(&f.id, &f.name, &f.encrypted); e != nil {
			rows.Close()
			return e
		}
		files = append(files, f)
	}
	rows.Close()
	for _, f := range files {
		plain, e := safeUploadPath(s.Data, f.name)
		if e != nil {
			return e
		}
		encrypted := plain + ".nc3e"
		if f.encrypted != target {
			if e = v.convertFile(plain, encrypted, f.name, target == 1); e != nil {
				return e
			}
			if _, e = s.DB.Exec("UPDATE files SET encrypted=? WHERE id=?", target, f.id); e != nil {
				return e
			}
		}
		if target == 1 {
			if e = os.Remove(plain); e != nil && !os.IsNotExist(e) {
				return e
			}
		} else {
			if e = os.Remove(encrypted); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	for _, name := range []string{"chat-before-v3.sqlite3", "chat-before-v6.sqlite3"} {
		backup := filepath.Join(s.Backups, name)
		context := "backup/" + strings.TrimSuffix(name, ".sqlite3")
		if _, e = os.Stat(backup); e == nil && target == 1 {
			if e = v.convertFile(backup, backup+".nc3e", context, true); e != nil {
				return e
			}
			if e = os.Remove(backup); e != nil {
				return e
			}
		} else if target == 0 {
			if _, e = os.Stat(backup + ".nc3e"); e == nil {
				if e = v.convertFile(backup, backup+".nc3e", context, false); e != nil {
					return e
				}
				if e = os.Remove(backup + ".nc3e"); e != nil {
					return e
				}
			}
		}
	}
	if _, e = s.DB.Exec("VACUUM"); e != nil {
		return e
	}
	if target == 1 {
		v.config.Phase = "ready"
		return v.save()
	}
	if e = os.Remove(v.path); e != nil {
		return e
	}
	wipe(v.key)
	v.key = nil
	v.config = nil
	return nil
}
func (v *Vault) convertFile(plain, encrypted, context string, encrypt bool) error {
	source, destination := plain, encrypted
	if !encrypt {
		source, destination = encrypted, plain
	}
	input, e := os.Open(source)
	if e != nil {
		return e
	}
	defer input.Close()
	output, e := os.CreateTemp(filepath.Dir(destination), ".vault-*")
	if e != nil {
		return e
	}
	temporary := output.Name()
	defer os.Remove(temporary)
	_ = output.Chmod(0600)
	if encrypt {
		_, e = v.encryptFile(output, input, context, 1<<40)
	} else {
		_, e = v.decryptFile(output, input, context)
	}
	if e == nil {
		e = output.Sync()
	}
	closeErr := output.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporary, destination)
}
