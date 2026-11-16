package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScreenSpecificDataAndHistoryPagination(t *testing.T) {
	s, _, server := testAPI(t)
	client := adminRemote(t, s, server)
	for i := 0; i < 25; i++ {
		_, err := s.DB.Exec("INSERT INTO messages(user_id,body,created_at) SELECT id,?,? FROM users WHERE username=?", fmt.Sprintf("message %d", i), timestamp(), adminUsername)
		if err != nil {
			t.Fatal(err)
		}
	}
	var page ChatData
	if err := client.call("GET", "/api/chat-data?section=chat&page_size=12", nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 12 || !page.More || len(page.Files) != 0 || page.Messages[11].Body != "message 24" {
		t.Fatalf("bad first page: %+v", page)
	}
	before := page.Messages[0].ID
	if err := client.call("GET", fmt.Sprintf("/api/chat-data?section=chat&page_size=12&before=%d", before), nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 12 || !page.More || page.Messages[11].ID >= before {
		t.Fatal("older-page boundary failed")
	}
	before = page.Messages[0].ID
	if err := client.call("GET", fmt.Sprintf("/api/chat-data?section=chat&page_size=12&before=%d", before), nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.More || page.Messages[0].Body != "message 0" {
		t.Fatal("last page lost messages")
	}
	// Invalid message bodies must never be read or decrypted by Files/People.
	if _, err := s.DB.Exec("UPDATE messages SET body='invalid ciphertext',body_encrypted=1"); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"files", "people"} {
		if err := client.call("GET", "/api/chat-data?section="+section, nil, &page); err != nil {
			t.Fatal(section, err)
		}
		if len(page.Messages) != 0 {
			t.Fatal("unneeded history returned")
		}
	}
	for _, query := range []string{"section=invalid", "page_size=0", "page_size=31", "page_size=bad"} {
		if err := client.call("GET", "/api/chat-data?"+query, nil, &page); err == nil {
			t.Fatal("invalid selector accepted", query)
		}
	}
}

func TestStaticCacheDoesNotAllocateSessions(t *testing.T) {
	s := testStore(t)
	app, err := newApp(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/app.css", "/app.js"} {
		request := httptest.NewRequest("GET", "https://chat.invalid"+path, nil)
		response := httptest.NewRecorder()
		app.web(response, request, false)
		etag := response.Header().Get("ETag")
		if response.Code != 200 || etag == "" || response.Body.Len() == 0 || response.Header().Get("Set-Cookie") != "" {
			t.Fatal("bad static response")
		}
		request.Header.Set("If-None-Match", etag)
		cached := httptest.NewRecorder()
		app.web(cached, request, false)
		if cached.Code != 304 || cached.Body.Len() != 0 {
			t.Fatal("static cache miss")
		}
	}
	if len(app.sessions) != 0 {
		t.Fatal("assets allocated web sessions")
	}
	response := httptest.NewRecorder()
	app.web(response, httptest.NewRequest("GET", "https://chat.invalid/login", nil), false)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private page became cacheable")
	}
}

func TestAPISessionChurnAndCrossOriginProtection(t *testing.T) {
	var sessions APISessions
	req := httptest.NewRequest("GET", "https://chat.invalid/api/session", nil)
	var cookie *http.Cookie
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		if _, ok := sessions.get(w, req); !ok {
			t.Fatal("budget exhausted early")
		}
		cookie = w.Result().Cookies()[0]
	}
	w := httptest.NewRecorder()
	if _, ok := sessions.get(w, req); ok || w.Code != 429 {
		t.Fatal("session churn was accepted")
	}
	req.AddCookie(cookie)
	if _, ok := sessions.get(httptest.NewRecorder(), req); !ok {
		t.Fatal("existing session blocked by churn")
	}
	s, api, server := testAPI(t)
	client := adminRemote(t, s, server)
	req = httptest.NewRequest("POST", server.URL+"/api/send", strings.NewReader(`{"body":"cross-origin"}`))
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS12}
	req.Header.Set("Origin", "https://other.invalid")
	req.Header.Set("X-CSRF-Token", client.CSRF)
	for _, c := range client.Client.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	w = httptest.NewRecorder()
	api.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("cross-origin API request accepted", w.Code)
	}
	var data ChatData
	if err := client.call("GET", "/api/chat-data", nil, &data); err != nil || len(data.Messages) != 0 {
		t.Fatal("cross-origin request changed history")
	}
}
