package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReader(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	s := testStore(t)
	a, err := newApp(s)
	if err != nil {
		t.Fatal(err)
	}
	a.shutdown = make(chan struct{})
	a.reader, err = newReaderBridge(a)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.web(w, r, true) }))
	_, a.localPort, _ = net.SplitHostPort(server.Listener.Addr().String())
	server.Start()
	t.Cleanup(func() { server.Close(); a.reader.close(); a.stopHost() })
	return a, server
}
func readerRequest(t *testing.T, a *App, server *httptest.Server, action *ReaderAction) *http.Response {
	t.Helper()
	method, path := "GET", "/_reader/state"
	var body io.Reader
	if action != nil {
		method, path = "POST", "/_reader/action"
		b, _ := json.Marshal(action)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, server.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+a.reader.token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func readerAction(t *testing.T, a *App, server *httptest.Server, action ReaderAction) ReaderState {
	t.Helper()
	response := readerRequest(t, a, server, &action)
	defer response.Body.Close()
	if response.StatusCode != 202 {
		b, _ := io.ReadAll(response.Body)
		t.Fatalf("action %s failed: %d %s", action.Action, response.StatusCode, b)
	}
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		s := a.reader.snapshot()
		if !s.Busy {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reader action timed out", action.Action)
	return ReaderState{}
}
func TestReaderPersistentConnectionPrivateMessagesFilesAndDisconnect(t *testing.T) {
	a, server := testReader(t)
	s := readerAction(t, a, server, ReaderAction{Action: "host"})
	if s.Error != "" || s.Stage != "connected" || !s.Hosting || !s.Admin {
		t.Fatalf("host: %+v", s)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "login", Username: "admin", Password: testPassword})
	if !s.Connected || !s.Admin {
		t.Fatalf("login: %+v", s)
	}
	insertUser(t, a.Store, "neighbour", "approved")
	job := ReaderAction{Action: "send", Body: "Still connected while reading", Token: strings.Repeat("r", 24)}
	s = readerAction(t, a, server, job)
	s = readerAction(t, a, server, job)
	if s.Error != "" || len(s.Messages) != 1 || s.SentToken != job.Token {
		t.Fatalf("send/retry: %+v", s)
	}
	// A fresh native window uses the same capability, not a new login cookie.
	response := readerRequest(t, a, server, nil)
	var reopened ReaderState
	if err := json.NewDecoder(response.Body).Decode(&reopened); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !reopened.Connected || len(reopened.Messages) != 1 {
		t.Fatal("reopening reset the session")
	}
	readerAction(t, a, server, ReaderAction{Action: "invite"})
	if a.reader.snapshot().JoinCode == "" {
		t.Fatal("invitation code missing")
	}
	file := filepath.Join(filepath.Dir(a.Store.Root), "shared-note.txt")
	if err := os.WriteFile(file, []byte("file from reader"), 0600); err != nil {
		t.Fatal(err)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "upload", Path: file})
	if s.Error != "" {
		t.Fatal(s.Error)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "files"})
	if len(s.Files) != 1 {
		t.Fatal("file list missing", s.Error)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "download", ID: s.Files[0].ID})
	if s.Error != "" {
		t.Fatal(s.Error)
	}
	downloads, _ := filepath.Glob(filepath.Join(filepath.Dir(a.Store.Root), "NeighbourChat-Downloads", "*"))
	if len(downloads) != 1 || readText(downloads[0]) != "file from reader" {
		t.Fatal("download failed")
	}
	s = readerAction(t, a, server, ReaderAction{Action: "disconnect"})
	if s.Connected || len(s.Messages) != 0 || !s.Hosting {
		t.Fatal("disconnect did not clear client while preserving host")
	}
	s = readerAction(t, a, server, ReaderAction{Action: "stop-host"})
	if s.Hosting {
		t.Fatal("host remained active")
	}
}

func TestReaderEndChatStopsOwnHostAndKeepsSavedData(t *testing.T) {
	a, server := testReader(t)
	s := readerAction(t, a, server, ReaderAction{Action: "host"})
	if s.Error != "" || !s.Hosting {
		t.Fatalf("host: %+v", s)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "login", Username: "admin", Password: testPassword})
	if s.Error != "" || !s.Connected || !s.Admin {
		t.Fatalf("login: %+v", s)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "end-chat"})
	if s.Error != "" || s.Hosting || s.Connected {
		t.Fatalf("end chat did not clean up: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(a.Store.Data, "chat.sqlite3")); err != nil {
		t.Fatalf("saved chat data was removed: %v", err)
	}
}

func TestReaderAuthenticationAndNonBlockingControl(t *testing.T) {
	a, server := testReader(t)
	response, err := server.Client().Get(server.URL + "/_reader/state")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 403 {
		t.Fatal("reader state exposed without capability")
	}
	response.Body.Close()
	req, _ := http.NewRequest("POST", server.URL+"/_reader/action", strings.NewReader(`{"action":"stop"}`))
	req.Header.Set("Origin", "https://evil.invalid")
	req.Header.Set("Authorization", "Bearer "+a.reader.token)
	response, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 403 {
		t.Fatal("cross-origin reader control accepted")
	}
	response.Body.Close()
	select {
	case <-a.shutdown:
		t.Fatal("cross-origin request stopped the service")
	default:
	}
	response = readerRequest(t, a, server, nil)
	etag := response.Header.Get("ETag")
	response.Body.Close()
	request, _ := http.NewRequest("GET", server.URL+"/_reader/state", nil)
	request.Header.Set("Authorization", "Bearer "+a.reader.token)
	request.Header.Set("If-None-Match", etag)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if etag == "" || response.StatusCode != 304 || len(content) != 0 {
		t.Fatal("unchanged cached state was retransmitted")
	}
	a.reader.change(func(s *ReaderState) { s.Busy = true })
	begin := time.Now()
	response = readerRequest(t, a, server, nil)
	response.Body.Close()
	if response.StatusCode != 200 || time.Since(begin) > time.Second {
		t.Fatal("state GET blocked on work")
	}
	response = readerRequest(t, a, server, &ReaderAction{Action: "stop"})
	response.Body.Close()
	select {
	case <-a.shutdown:
	case <-time.After(time.Second):
		t.Fatal("stop was blocked by busy operation")
	}
}

func TestReaderPinnedJoinRegistrationAndSuspendRetainSession(t *testing.T) {
	a, server := testReader(t)
	store, _, remoteServer := testAPI(t)
	s := readerAction(t, a, server, ReaderAction{Action: "probe", Address: remoteServer.URL})
	if s.Error != "" || s.Stage != "verify" || s.Code != checkCode(store.Fingerprint) {
		t.Fatalf("probe: %+v", s)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "confirm", Token: "wrong"})
	if s.Error == "" {
		t.Fatal("wrong verification accepted")
	}
	s = readerAction(t, a, server, ReaderAction{Action: "confirm", Token: s.VerifyToken})
	if s.Error != "" || s.Stage != "login" {
		t.Fatalf("confirm: %+v", s)
	}
	s = readerAction(t, a, server, ReaderAction{Action: "login", Username: "admin", Password: testPassword})
	if !s.Connected {
		t.Fatal(s.Error)
	}
	for _, action := range []string{"pause", "resume"} {
		response := readerRequest(t, a, server, &ReaderAction{Action: action})
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal(action)
		}
	}
	if !a.reader.snapshot().Connected || a.reader.snapshot().Paused {
		t.Fatal("suspend reset the connection")
	}
	if token := readText(filepath.Join(a.Store.Data, "koreader-token.txt")); len(token) != 43 {
		t.Fatal("missing reader capability")
	}
}

func TestReaderStopCommandEndsActualController(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	finished := make(chan error, 1)
	go func() { finished <- run([]string{"serve", "--root", root, "--local-port", port}) }()
	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	ready := false
	for until := time.Now().Add(8 * time.Second); time.Now().Before(until); {
		response, e := client.Get(base + "/_health")
		if e == nil {
			response.Body.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("controller never started")
	}
	token := readText(filepath.Join(root, "data", "koreader-token.txt"))
	req, _ := http.NewRequest("POST", base+"/_reader/action", strings.NewReader(`{"action":"stop"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case err = <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("controller remained running")
	}
	if _, err = os.Stat(filepath.Join(root, "data", "server.pid")); !os.IsNotExist(err) {
		t.Fatal("PID file remained")
	}
	if _, err = client.Get(base + "/_health"); err == nil {
		t.Fatal("controller still accepts connections")
	}
}

func TestReaderIncrementalPollAndAdaptiveIdleInterval(t *testing.T) {
	a, server := testReader(t)
	readerAction(t, a, server, ReaderAction{Action: "host"})
	readerAction(t, a, server, ReaderAction{Action: "login", Username: "admin", Password: testPassword})
	for i := 0; i < 15; i++ {
		readerAction(t, a, server, ReaderAction{Action: "send", Body: "Message " + strings.Repeat("x", i), Token: strings.Repeat("i", 20) + string(rune('a'+i))})
	}
	s := a.reader.snapshot()
	if len(s.Messages) != 12 || !s.More {
		t.Fatal("latest page did not stay bounded")
	}
	before := s.Before
	// Force a regular background-style incremental refresh, with no new messages.
	if err := a.reader.refresh(); err != nil {
		t.Fatal(err)
	}
	if got := a.reader.snapshot(); len(got.Messages) != 12 || got.Before != before || !got.More {
		t.Fatal("empty incremental poll lost cached history")
	}
	if interval := a.reader.pollInterval(); interval != 30*time.Second {
		t.Fatal("hidden UI still polls at foreground rate", interval)
	}
	response := readerRequest(t, a, server, nil)
	response.Body.Close()
	if interval := a.reader.pollInterval(); interval != 10*time.Second {
		t.Fatal("visible UI did not use foreground interval", interval)
	}
	_, err := a.Store.DB.Exec("INSERT INTO messages(user_id,body,created_at) SELECT id,'new reply',? FROM users WHERE username=?", timestamp(), adminUsername)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.reader.refresh(); err != nil {
		t.Fatal(err)
	}
	if got := a.reader.snapshot(); len(got.Messages) != 12 || got.Messages[0].Body != "new reply" || !got.More {
		t.Fatal("new reply did not merge into bounded cache")
	}
	// Revocation must be detected without a separate /session request every poll.
	a.Store.mu.Lock()
	a.Store.AdminVersion++
	a.Store.mu.Unlock()
	if err = a.reader.refresh(); err != nil {
		t.Fatal(err)
	}
	if got := a.reader.snapshot(); got.Connected || len(got.Messages) != 0 {
		t.Fatal("revoked session retained private cache")
	}
}
