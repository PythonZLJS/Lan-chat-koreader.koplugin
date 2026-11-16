package main

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPassword = "correct-test-password"
const legacyScrypt = "scrypt:32768:8:1$2Lgz7cBQ5GzSG7jp$bf28bca0fc79cd9f53cdc412c5f7ad55f2fecdc0d59c1ec0c1e871c785671ce4ed345ac873c9fc2716e776ff141f62124d4246ee4e39f386beeeaea4dfc46f74"
const legacyPBKDF = "pbkdf2:sha256:600000$OSpJaErEpBsduCDI$bfdc612cb4f56e5b2af3d1c9497eb5ef3aa4379585192ef9eeda139ded504e97"

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := openStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Close() })
	initial := readText(filepath.Join(s.Data, "initial-admin-password.txt"))
	if e = s.changeAdmin(initial, testPassword); e != nil {
		t.Fatal(e)
	}
	return s
}
func testAPI(t *testing.T) (*Store, *API, *httptest.Server) {
	t.Helper()
	s := testStore(t)
	a := newAPI(s)
	server := httptest.NewUnstartedServer(a)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{s.Cert}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return s, a, server
}
func adminRemote(t *testing.T, s *Store, server *httptest.Server) *Remote {
	t.Helper()
	r := newRemote(server.URL, s.Fingerprint)
	if _, e := r.info(); e != nil {
		t.Fatal(e)
	}
	if e := r.call("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil); e != nil {
		t.Fatal(e)
	}
	return r
}
func insertUser(t *testing.T, s *Store, name, status string) int64 {
	t.Helper()
	hash, e := hashPassword(testPassword)
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.DB.Exec("INSERT INTO users(username,display_name,password_hash,status,created_at) VALUES(?,?,?,?,?)", name, strings.Title(name), hash, status, timestamp())
	if e != nil {
		t.Fatal(e)
	}
	id, _ := result.LastInsertId()
	return id
}
func userRemote(t *testing.T, s *Store, server *httptest.Server, name string) *Remote {
	t.Helper()
	r := newRemote(server.URL, s.Fingerprint)
	if _, e := r.info(); e != nil {
		t.Fatal(e)
	}
	if e := r.call("POST", "/api/login", map[string]string{"username": name, "password": testPassword}, nil); e != nil {
		t.Fatal(e)
	}
	return r
}

func TestPasswordsAndLegacyCompatibility(t *testing.T) {
	hash, e := hashPassword(testPassword)
	if e != nil {
		t.Fatal(e)
	}
	if !verifyPassword(hash, testPassword) || verifyPassword(hash, "wrong") {
		t.Fatal("Argon2 password verification failed")
	}
	other, _ := hashPassword(testPassword)
	if hash == other {
		t.Fatal("salts must be unique")
	}
	for _, hash := range []string{legacyScrypt, legacyPBKDF} {
		if !verifyPassword(hash, "legacy-test-password") || verifyPassword(hash, "wrong") {
			t.Fatal("legacy password verification failed")
		}
	}
	for _, hash := range []string{"bad", "$argon2id$v=19$m=999999,t=99,p=9$a$b", "scrypt:999999999:8:9$salt$deadbeef", strings.Repeat("x", 1025)} {
		if verifyPassword(hash, testPassword) {
			t.Fatal("malformed hash was accepted")
		}
	}
}
func TestPerDeviceCertificatesPersist(t *testing.T) {
	s := testStore(t)
	first := s.Fingerprint
	other := testStore(t)
	if first == other.Fingerprint {
		t.Fatal("different installations must not share TLS private keys")
	}
	if e := s.loadCertificate(); e != nil || s.Fingerprint != first {
		t.Fatal("certificate identity changed on restart", e)
	}
}
func TestLegacyMigrationPreservesMessagesAccountsFilesAndPasswords(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if e := os.MkdirAll(filepath.Join(data, "uploads"), 0700); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(data, "chat.sqlite3"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE users(id INTEGER PRIMARY KEY,username TEXT UNIQUE NOT NULL,display_name TEXT NOT NULL,password_hash TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',created_at TEXT NOT NULL);CREATE TABLE messages(id INTEGER PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),recipient_id INTEGER REFERENCES users(id),body TEXT NOT NULL,created_at TEXT NOT NULL);CREATE TABLE files(id INTEGER PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id),recipient_id INTEGER REFERENCES users(id),stored_name TEXT NOT NULL UNIQUE,original_name TEXT NOT NULL,size INTEGER NOT NULL,created_at TEXT NOT NULL);`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO users VALUES(7,'reader','Old Reader',?,'approved',?);INSERT INTO messages VALUES(31,7,NULL,'Keep this chat',?);INSERT INTO files VALUES(11,7,NULL,'old-file','book.txt',4,?)", legacyScrypt, timestamp(), timestamp(), timestamp())
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	_ = os.WriteFile(filepath.Join(data, "admin-password.hash"), []byte(legacyScrypt), 0600)
	_ = os.WriteFile(filepath.Join(data, "join-code.txt"), []byte("EXISTING-CODE"), 0600)
	_ = os.WriteFile(filepath.Join(data, "uploads", "old-file"), []byte("keep"), 0600)
	s, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if !verifyPassword(s.AdminHash, "legacy-test-password") || s.joinCode() != "EXISTING-CODE" {
		t.Fatal("old settings changed")
	}
	var body string
	if s.DB.QueryRow("SELECT body FROM messages WHERE id=31").Scan(&body) != nil || body != "Keep this chat" {
		t.Fatal("old message lost")
	}
	if readText(filepath.Join(data, "uploads", "old-file")) != "keep" {
		t.Fatal("old upload lost")
	}
	backup, e := sql.Open("sqlite", filepath.Join(s.Backups, "chat-before-v3.sqlite3"))
	if e != nil {
		t.Fatal(e)
	}
	defer backup.Close()
	var count int
	if e = backup.QueryRow("SELECT count(*) FROM messages").Scan(&count); e != nil || count != 1 {
		t.Fatal("consistent backup missing", e)
	}
}
func TestRegistrationApprovalAndRevocation(t *testing.T) {
	s, a, server := testAPI(t)
	r := newRemote(server.URL, s.Fingerprint)
	_, _ = r.info()
	e := r.call("POST", "/api/register", map[string]string{"username": "reader", "display": "Reader", "password": testPassword, "join_code": s.joinCode()}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.call("POST", "/api/login", map[string]string{"username": "reader", "password": testPassword}, nil) == nil {
		t.Fatal("pending account signed in")
	}
	admin := adminRemote(t, s, server)
	var data AdminData
	if e = admin.call("GET", "/api/admin", nil, &data); e != nil || len(data.Pending) != 1 {
		t.Fatal(e, data)
	}
	id := data.Pending[0].ID
	if e = admin.call("POST", "/api/admin/action", map[string]any{"action": "approve", "id": id}, nil); e != nil {
		t.Fatal(e)
	}
	reader := userRemote(t, s, server, "reader")
	if e = reader.call("POST", "/api/admin/action", map[string]string{"action": "history", "days": "1"}, nil); e == nil {
		t.Fatal("non-admin changed host settings")
	}
	if e = admin.call("POST", "/api/admin/action", map[string]any{"action": "disable", "id": id}, nil); e != nil {
		t.Fatal(e)
	}
	info, e := reader.info()
	if e != nil || info.Authenticated {
		t.Fatal("disabled user session still active", e)
	}
	_ = a
}
func TestMessagePrivacySearchAndDuplicatePrevention(t *testing.T) {
	s, _, server := testAPI(t)
	aliceID := insertUser(t, s, "alice", "approved")
	bobID := insertUser(t, s, "bobby", "approved")
	_ = insertUser(t, s, "carol", "approved")
	alice := userRemote(t, s, server, "alice")
	bob := userRemote(t, s, server, "bobby")
	carol := userRemote(t, s, server, "carol")
	payload := map[string]any{"body": "private secret needle", "recipient_id": fmt.Sprint(bobID), "request_id": "same-request-token-123456789"}
	for n := 0; n < 2; n++ {
		if e := alice.call("POST", "/api/send", payload, nil); e != nil {
			t.Fatal(e)
		}
	}
	if e := alice.call("POST", "/api/send", map[string]string{"body": "public hello"}, nil); e != nil {
		t.Fatal(e)
	}
	for _, r := range []*Remote{alice, bob} {
		var data ChatData
		if e := r.call("GET", "/api/chat-data", nil, &data); e != nil || len(data.Messages) != 2 {
			t.Fatal("participants could not see private message", e, data)
		}
	}
	var other ChatData
	if e := carol.call("GET", "/api/chat-data", nil, &other); e != nil || len(other.Messages) != 1 || other.Messages[0].Body != "public hello" {
		t.Fatal("private message leaked", e, other)
	}
	var result struct{ Results []Message }
	if e := carol.call("GET", "/api/search?q=needle", nil, &result); e != nil || len(result.Results) != 0 {
		t.Fatal("private search leaked", e)
	}
	var count int
	_ = s.DB.QueryRow("SELECT count(*) FROM messages WHERE user_id=?", aliceID).Scan(&count)
	if count != 2 {
		t.Fatal("message retry created duplicate", count)
	}
}
func TestFileStreamingPrivacyTraversalAndSizeLimit(t *testing.T) {
	s, a, server := testAPI(t)
	_ = insertUser(t, s, "alice", "approved")
	bobID := insertUser(t, s, "bobby", "approved")
	_ = insertUser(t, s, "carol", "approved")
	alice := userRemote(t, s, server, "alice")
	bob := userRemote(t, s, server, "bobby")
	carol := userRemote(t, s, server, "carol")
	if e := alice.upload("note.txt", fmt.Sprint(bobID), strings.NewReader("private-file"), nil); e != nil {
		t.Fatal(e)
	}
	var id int64
	_ = s.DB.QueryRow("SELECT id FROM files").Scan(&id)
	response, e := bob.request("GET", fmt.Sprint("/api/download/", id), nil, "")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(b) != "private-file" {
		t.Fatal("recipient download failed")
	}
	response, e = carol.request("GET", fmt.Sprint("/api/download/", id), nil, "")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("file leaked to another account")
	}
	for _, name := range []string{"../secret", "..\\secret", "/etc/passwd", "a:b", "."} {
		if _, e := safeUploadPath(s.Data, name); e == nil {
			t.Fatal("unsafe path accepted", name)
		}
	}
	a.mu.Lock()
	a.UploadLimit = 4
	a.mu.Unlock()
	if alice.upload("big.txt", "", strings.NewReader("too-large"), nil) == nil {
		t.Fatal("oversized file accepted")
	}
	var count int
	_ = s.DB.QueryRow("SELECT count(*) FROM files").Scan(&count)
	if count != 1 {
		t.Fatal("failed upload was stored")
	}
	files, _ := os.ReadDir(filepath.Join(s.Data, "uploads"))
	if len(files) != 1 {
		t.Fatal("failed upload left a file behind")
	}
}
func TestCSRFAndSessionRotationAndAdminPasswordChange(t *testing.T) {
	s, _, server := testAPI(t)
	r := newRemote(server.URL, s.Fingerprint)
	_, _ = r.info()
	u, _ := url.Parse(server.URL)
	before := r.Client.Jar.Cookies(u)[0].Value
	r.CSRF = "wrong"
	if r.call("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil) == nil {
		t.Fatal("invalid CSRF accepted")
	}
	_, _ = r.info()
	if e := r.call("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil); e != nil {
		t.Fatal(e)
	}
	after := r.Client.Jar.Cookies(u)[0].Value
	if before == after {
		t.Fatal("session ID was not rotated")
	}
	second := adminRemote(t, s, server)
	if e := r.call("POST", "/api/admin/password", map[string]string{"current_password": testPassword, "new_password": "replacement-test-password"}, nil); e != nil {
		t.Fatal(e)
	}
	info, e := second.info()
	if e != nil || info.Authenticated {
		t.Fatal("old admin session survived password change")
	}
	if _, e = os.Stat(filepath.Join(s.Data, "initial-admin-password.txt")); !os.IsNotExist(e) {
		t.Fatal("initial password file retained")
	}
}
func TestFingerprintPinCheckedBeforeSendingCredentials(t *testing.T) {
	s := testStore(t)
	var seen atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		writeJSON(w, 200, map[string]string{"csrf": "test"})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{s.Cert}}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	pin, e := probeCertificate(server.URL)
	if e != nil || pin != s.Fingerprint || seen.Load() != 0 {
		t.Fatal("certificate probe sent application data", e)
	}
	bad := newRemote(server.URL, strings.Repeat("0", 64))
	bad.CSRF = "token"
	if bad.call("POST", "/api/login", map[string]string{"password": "must-not-send"}, nil) == nil {
		t.Fatal("wrong certificate accepted")
	}
	if seen.Load() != 0 {
		t.Fatal("credentials sent before pin check")
	}
}
func TestAddressesRestrictPublicNetworkAndURLInjection(t *testing.T) {
	for _, raw := range []string{"http://192.168.1.2:8000", "https://8.8.8.8", "https://example.com", "https://192.168.1.2@8.8.8.8", "https://169.254.169.254", "https://192.168.1.2/?token=x", "https://192.168.1.2/#fragment", "https://192.168.1.2/other"} {
		if _, e := privateOrigin(raw); e == nil {
			t.Fatal("unsafe address accepted", raw)
		}
	}
	if got, e := privateOrigin("192.168.1.42:8000"); e != nil || got != "https://192.168.1.42:8000" {
		t.Fatal(got, e)
	}
}
func TestExpiredAndSingleUseLaunchTicketAndLoopbackGuard(t *testing.T) {
	s := testStore(t)
	app, e := newApp(s)
	if e != nil {
		t.Fatal(e)
	}
	ticket, e := writeTicket(s.Data)
	if e != nil {
		t.Fatal(e)
	}
	makeLaunch := func(remote, host, token string) *httptest.ResponseRecorder {
		form := url.Values{"token": {token}}
		r := httptest.NewRequest("POST", "http://"+host+"/_launch", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		app.web(w, r, true)
		return w
	}
	if makeLaunch("192.168.1.9:1234", "127.0.0.1:8001", ticket.Token).Code != 403 {
		t.Fatal("LAN access to controller allowed")
	}
	if makeLaunch("127.0.0.1:1234", "evil.invalid:8001", ticket.Token).Code != 403 {
		t.Fatal("Host rebinding allowed")
	}
	if makeLaunch("127.0.0.1:1234", "127.0.0.1:8001", ticket.Token).Code != 303 {
		t.Fatal("valid ticket rejected")
	}
	if makeLaunch("127.0.0.1:1234", "127.0.0.1:8001", ticket.Token).Code != 403 {
		t.Fatal("launch ticket reused")
	}
	ticket.Expires = time.Now().Add(-time.Minute).Unix()
	b, _ := json.Marshal(ticket)
	_ = os.WriteFile(filepath.Join(s.Data, "launch-ticket.json"), b, 0600)
	if makeLaunch("127.0.0.1:1234", "127.0.0.1:8001", ticket.Token).Code != 403 {
		t.Fatal("expired ticket accepted")
	}
}
func TestIllusionRegistrationUninstallPreservesOtherAppsAndChatData(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "appreg.db")
	db, e := sql.Open("sqlite", registry)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("CREATE TABLE interfaces(interface TEXT PRIMARY KEY);CREATE TABLE handlerIds(handlerId TEXT PRIMARY KEY);CREATE TABLE properties(handlerId TEXT,name TEXT,value TEXT,PRIMARY KEY(handlerId,name));INSERT INTO handlerIds VALUES('another.app');")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(root, "mesquite", "neighbour-chat")
	for n := 0; n < 2; n++ {
		if e = prepare(root, target, registry); e != nil {
			t.Fatal(e)
		}
	}
	marker := filepath.Join(root, "data", "keep.txt")
	_ = os.WriteFile(marker, []byte("keep my chats"), 0600)
	if e = uninstall(root, target, registry); e != nil {
		t.Fatal(e)
	}
	if readText(marker) != "keep my chats" {
		t.Fatal("uninstaller deleted data")
	}
	db, e = sql.Open("sqlite", registry)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var count int
	_ = db.QueryRow("SELECT count(*) FROM handlerIds WHERE handlerId='another.app'").Scan(&count)
	if count != 1 {
		t.Fatal("uninstaller changed another app")
	}
	_ = db.QueryRow("SELECT count(*) FROM handlerIds WHERE handlerId=?", appID).Scan(&count)
	if count != 0 {
		t.Fatal("app not unregistered")
	}
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func browserPost(t *testing.T, c *http.Client, address, path string, values url.Values) string {
	t.Helper()
	response, e := c.PostForm(address+path, values)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 400 {
		t.Fatalf("browser POST %s: %d %s", path, response.StatusCode, b)
	}
	return string(b)
}
func pageCSRF(t *testing.T, body string) string {
	t.Helper()
	m := csrfPattern.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatal("CSRF field not present", truncate(body, 300))
	}
	return m[1]
}
func TestRealBrowserFlowRendersTrustedUIAndRejectsCrossOriginForms(t *testing.T) {
	store, _, apiServer := testAPI(t)
	insertUser(t, store, "reader", "approved")
	app, e := newApp(store)
	if e != nil {
		t.Fatal(e)
	}
	local := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.web(w, r, true) }))
	_, port, _ := net.SplitHostPort(local.Listener.Addr().String())
	app.localPort = port
	local.Start()
	defer local.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	ticket, _ := writeTicket(store.Data)
	home := browserPost(t, client, local.URL, "/_launch", url.Values{"token": {ticket.Token}})
	token := pageCSRF(t, home)
	confirm := browserPost(t, client, local.URL, "/join", url.Values{"csrf": {token}, "address": {apiServer.URL}})
	if !strings.Contains(confirm, checkCode(store.Fingerprint)) {
		t.Fatal("server check code missing")
	}
	login := browserPost(t, client, local.URL, "/confirm", url.Values{"csrf": {token}, "confirm": {"yes"}})
	if !strings.Contains(login, "Sign in") {
		t.Fatal("not redirected to sign in")
	}
	chat := browserPost(t, client, local.URL, "/login", url.Values{"csrf": {token}, "username": {"admin"}, "password": {testPassword}})
	if !strings.Contains(chat, "Host tools") || !strings.Contains(chat, `action="/send"`) {
		t.Fatal("chat did not render")
	}
	token = pageCSRF(t, chat)
	noncePattern := regexp.MustCompile(`name="request_id" value="([^"]+)"`)
	nonce := noncePattern.FindStringSubmatch(chat)[1]
	body := browserPost(t, client, local.URL, "/send", url.Values{"csrf": {token}, "request_id": {nonce}, "body": {"<script>must be text</script>"}})
	if !strings.Contains(body, "&lt;script&gt;must be text&lt;/script&gt;") {
		t.Fatal("message HTML was not escaped")
	}
	if strings.Contains(body, "<script>must be text") {
		t.Fatal("message script executable")
	}
	request, _ := http.NewRequest("POST", local.URL+"/host", strings.NewReader(url.Values{"csrf": {token}, "action": {"stop"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.invalid")
	response, e := client.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin form accepted")
	}
}
func TestNoHTMLProxyAndRedirectsAreNotFollowed(t *testing.T) {
	s := testStore(t)
	var other atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer external.Close()
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("html") == "yes" {
			fmt.Fprint(w, "<script>unsafe()</script>")
			return
		}
		http.Redirect(w, r, external.URL, 302)
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{s.Cert}}
	upstream.StartTLS()
	defer upstream.Close()
	client := newRemote(upstream.URL, s.Fingerprint)
	if _, e := client.info(); e == nil {
		t.Fatal("unexpected redirect accepted")
	}
	if e := client.call("GET", "/api/session?html=yes", nil, nil); e == nil {
		t.Fatal("upstream HTML accepted as API data")
	}
	if other.Load() != 0 {
		t.Fatal("redirect sent a request to another host")
	}
}
func TestMultipartInvalidCSRFDoesNotSpoolFiles(t *testing.T) {
	s := testStore(t)
	app, _ := newApp(s)
	session := &WebSession{ID: "test", CSRF: "correct", Local: true, Expires: time.Now().Add(time.Hour), Sent: map[string]bool{}}
	app.sessions[session.ID] = session
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("csrf", "wrong")
	part, _ := writer.CreateFormFile("file", "big.txt")
	_, _ = part.Write(bytes.Repeat([]byte("x"), 1<<20))
	writer.Close()
	request := httptest.NewRequest("POST", "http://127.0.0.1:8001/upload", &body)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(&http.Cookie{Name: "nc_local", Value: session.ID})
	recorder := httptest.NewRecorder()
	app.web(recorder, request, true)
	if recorder.Code != 400 {
		t.Fatal("invalid multipart token accepted")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Data, "uploads"))
	if len(entries) != 0 {
		t.Fatal("unauthorized upload stored a file")
	}
}
func TestRateLimiterIsBounded(t *testing.T) {
	var limiter Limiter
	for i := 0; i < 5; i++ {
		if !limiter.Allow("one", 5, time.Minute) {
			t.Fatal("early limit")
		}
	}
	if limiter.Allow("one", 5, time.Minute) {
		t.Fatal("rate limit exceeded")
	}
}
func TestRestartAndUninstallDestination(t *testing.T) {
	s := testStore(t)
	root := s.Root
	pin := s.Fingerprint
	join := s.JoinCode
	s.DB.Close()
	restored, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.DB.Close()
	if restored.Fingerprint != pin || restored.JoinCode != join || !verifyPassword(restored.AdminHash, testPassword) {
		t.Fatal("settings changed on restart")
	}
	app, e := newApp(restored)
	if e != nil {
		t.Fatal(e)
	}
	ticket, _ := writeTicket(restored.Data)
	ticket.Next = "/uninstall"
	body, _ := json.Marshal(ticket)
	_ = os.WriteFile(filepath.Join(restored.Data, "launch-ticket.json"), body, 0600)
	form := url.Values{"token": {ticket.Token}}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8001/_launch", strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.web(w, r, true)
	if w.Code != 303 || w.Header().Get("Location") != "/uninstall" {
		t.Fatal("uninstall launcher did not open confirmation screen")
	}
	cookies := w.Result().Cookies()
	request := httptest.NewRequest("GET", "http://127.0.0.1:8001/uninstall", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	confirmation := httptest.NewRecorder()
	app.web(confirmation, request, true)
	if confirmation.Code != 200 || !strings.Contains(confirmation.Body.String(), "Uninstall Neighbour Chat?") {
		t.Fatal("uninstall confirmation failed")
	}
}
func TestOwnBackendDoesNotDependOnPublicConnectionSlots(t *testing.T) {
	store := testStore(t)
	app, e := newApp(store)
	if e != nil {
		t.Fatal(e)
	}
	app.ListenerAddr = "https://127.0.0.1:1"
	client := app.ownRemote()
	if _, e = client.info(); e != nil {
		t.Fatal("own backend tried to use the public network listener", e)
	}
	if e = client.call("POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil); e != nil {
		t.Fatal(e)
	}
	if e = client.call("POST", "/api/send", map[string]string{"body": "Internal streaming backend"}, nil); e != nil {
		t.Fatal(e)
	}
	var data ChatData
	if e = client.call("GET", "/api/chat-data", nil, &data); e != nil || len(data.Messages) != 1 {
		t.Fatal("internal API did not return data", e)
	}
}
func TestUninstallRequiresActualOwnedExecutableAndMode(t *testing.T) {
	path := "/mnt/us/lan_chat/neighbour-chat"
	legacy := "/mnt/us/lan_chat/app_kindle.py"
	for _, raw := range []string{"sha256sum\x00" + path + "\x00", path + "\x00version\x00", "/bin/other\x00serve\x00" + path + "\x00"} {
		if ownedCommand([]byte(raw), []string{path, legacy}) {
			t.Fatal("unrelated process was marked as owned")
		}
	}
	if !ownedCommand([]byte(path+"\x00serve\x00--root\x00/mnt/us/lan_chat\x00"), []string{path}) {
		t.Fatal("own server not recognized")
	}
	if !ownedCommand([]byte("/mnt/us/lan_chat/python/bin/python3.13\x00"+legacy+"\x00"), []string{legacy}) {
		t.Fatal("legacy server not recognized")
	}
}
