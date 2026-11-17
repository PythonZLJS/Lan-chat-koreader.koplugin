package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

var passwordSlots = make(chan struct{}, 2)

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func validPasswordInput(s string) bool {
	return utf8.ValidString(s) && len(s) <= 1024 && utf8.RuneCountInString(s) <= 256
}
func validPassword(s string) bool { return validPasswordInput(s) && utf8.RuneCountInString(s) >= 10 }
func hashPassword(password string) (string, error) {
	if !validPassword(password) {
		return "", errors.New("Use a password of 10 to 256 characters.")
	}
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots }()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func verifyPassword(encoded, password string) bool {
	if !validPasswordInput(password) || len(encoded) > 1024 {
		return false
	}
	passwordSlots <- struct{}{}
	defer func() { <-passwordSlots }()
	if strings.HasPrefix(encoded, "$argon2id$") {
		p := strings.Split(encoded, "$")
		if len(p) != 6 || p[2] != "v=19" || p[3] != "m=19456,t=2,p=1" {
			return false
		}
		salt, e := base64.RawStdEncoding.DecodeString(p[4])
		if e != nil || len(salt) != 16 {
			return false
		}
		want, e := base64.RawStdEncoding.DecodeString(p[5])
		if e != nil || len(want) != 32 {
			return false
		}
		return subtle.ConstantTimeCompare(want, argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)) == 1
	}
	// Preserve existing Werkzeug passwords during the SQLite-data upgrade.
	p := strings.Split(encoded, "$")
	if len(p) != 3 || len(p[1]) > 128 {
		return false
	}
	want, e := hex.DecodeString(p[2])
	if e != nil || len(want) > 64 || len(want) < 16 {
		return false
	}
	method := strings.Split(p[0], ":")
	var key []byte
	if len(method) == 4 && method[0] == "scrypt" {
		n, e1 := strconv.Atoi(method[1])
		r, e2 := strconv.Atoi(method[2])
		parallel, e3 := strconv.Atoi(method[3])
		if e1 != nil || e2 != nil || e3 != nil || n < 16384 || n > 131072 || n&(n-1) != 0 || r != 8 || parallel < 1 || parallel > 10 || int64(n)*int64(r)*int64(parallel) > 1048576 {
			return false
		}
		key, e = scrypt.Key([]byte(password), []byte(p[1]), n, r, parallel, len(want))
		if e != nil {
			return false
		}
	} else if len(method) == 3 && method[0] == "pbkdf2" && method[1] == "sha256" {
		rounds, err := strconv.Atoi(method[2])
		if err != nil || rounds < 100000 || rounds > 2000000 {
			return false
		}
		key = pbkdf2.Key([]byte(password), []byte(p[1]), rounds, len(want), sha256.New)
	} else {
		return false
	}
	return subtle.ConstantTimeCompare(key, want) == 1
}

type Bucket struct {
	Start time.Time
	Count int
}
type Limiter struct {
	mu      sync.Mutex
	entries map[string]Bucket
}

func (l *Limiter) Allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]Bucket{}
	}
	now := time.Now()
	b := l.entries[key]
	if now.Sub(b.Start) > window {
		b = Bucket{Start: now}
	}
	if len(l.entries) > 4096 {
		for k, v := range l.entries {
			if now.Sub(v.Start) > time.Hour {
				delete(l.entries, k)
			}
		}
		if len(l.entries) > 4096 {
			return false
		}
	}
	b.Count++
	l.entries[key] = b
	return b.Count <= max
}

type APISession struct {
	ID, CSRF string
	UID      int64
	Admin    bool
	Version  int64
	Expires  time.Time
}
type APISessions struct {
	mu      sync.Mutex
	entries map[string]APISession
	newRate Limiter
	ownerID string
}

func (s *APISessions) get(w http.ResponseWriter, r *http.Request) (APISession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]APISession{}
	}
	now := time.Now()
	c, _ := r.Cookie("nc_api")
	if c != nil {
		if v, ok := s.entries[c.Value]; ok && now.Before(v.Expires) {
			return v, true
		}
	}
	// Reject session churn before allocating tokens or filling the global cap.
	// Existing sessions keep working even when a peer exhausts this budget.
	if !s.newRate.Allow(clientIP(r), 20, time.Minute) {
		http.Error(w, "Too many new sessions. Try again shortly.", http.StatusTooManyRequests)
		return APISession{}, false
	}
	for k, v := range s.entries {
		if now.After(v.Expires) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= 512 {
		for key, value := range s.entries {
			if value.UID == 0 {
				delete(s.entries, key)
				break
			}
		}
	}
	if len(s.entries) >= 512 {
		http.Error(w, "Too many sessions. Try again later.", 503)
		return APISession{}, false
	}
	v := APISession{ID: randomToken(32), CSRF: randomToken(32), Expires: now.Add(12 * time.Hour)}
	s.entries[v.ID] = v
	http.SetCookie(w, &http.Cookie{Name: "nc_api", Value: v.ID, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	return v, true
}
func (s *APISessions) rotate(w http.ResponseWriter, old APISession, uid int64, admin bool, version int64) (APISession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.entries[old.ID]
	if !exists || !time.Now().Before(current.Expires) {
		return APISession{}, false
	}
	if old.ID == s.ownerID && !admin && len(s.entries) > 512 {
		return APISession{}, false
	}
	delete(s.entries, old.ID)
	v := APISession{ID: randomToken(32), CSRF: randomToken(32), UID: uid, Admin: admin, Version: version, Expires: time.Now().Add(12 * time.Hour)}
	s.entries[v.ID] = v
	http.SetCookie(w, &http.Cookie{Name: "nc_api", Value: v.ID, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	if old.ID == s.ownerID {
		if admin {
			s.ownerID = v.ID
		} else {
			s.ownerID = ""
		}
	}
	return v, true
}
func (s *APISessions) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
	if s.ownerID == id {
		s.ownerID = ""
	}
}
func clientIP(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	return host
}
func localRequest(r *http.Request) bool {
	ip := net.ParseIP(clientIP(r))
	return ip != nil && ip.IsLoopback()
}
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// One reserved local-owner slot. LAN churn cannot lock the device owner out.
// This is only called by the capability-protected controller, never by API routes.
func (s *APISessions) grantOwner(uid, version int64) APISession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]APISession{}
	}
	now := time.Now()
	if current, ok := s.entries[s.ownerID]; ok && now.Before(current.Expires) {
		return current
	}
	delete(s.entries, s.ownerID)
	for id, session := range s.entries {
		if !now.Before(session.Expires) {
			delete(s.entries, id)
		}
	}
	session := APISession{ID: randomToken(32), CSRF: randomToken(32), UID: uid, Admin: true, Version: version, Expires: now.Add(12 * time.Hour)}
	s.entries[session.ID] = session
	s.ownerID = session.ID
	return session
}
