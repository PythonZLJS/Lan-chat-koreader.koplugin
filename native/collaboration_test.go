package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hostAction(t *testing.T, r *Remote, action string, id int64, room string) {
	t.Helper()
	if e := r.call("POST", "/api/host/action", map[string]any{"action": action, "id": id, "room": room}, nil); e != nil {
		t.Fatal(action, e)
	}
}
func chatFor(t *testing.T, r *Remote) ChatData {
	t.Helper()
	var d ChatData
	if e := r.call("GET", "/api/chat-data", nil, &d); e != nil {
		t.Fatal(e)
	}
	return d
}
func sendText(t *testing.T, r *Remote, body, recipient string) {
	t.Helper()
	if e := r.call("POST", "/api/send", map[string]string{"body": body, "recipient_id": recipient, "request_id": randomToken(24)}, nil); e != nil {
		t.Fatal(e)
	}
}
func TestRoomsIsolateMessagesSearchFilesAndKeepPrivateDelivery(t *testing.T) {
	s, _, server := testAPI(t)
	admin := adminRemote(t, s, server)
	aliceID := insertUser(t, s, "alice", "approved")
	insertUser(t, s, "bob", "approved")
	alice := userRemote(t, s, server, "alice")
	bob := userRemote(t, s, server, "bob")
	hostAction(t, admin, "create-room", 0, "Reading")
	sendText(t, bob, "lobby secret", "")
	if e := bob.upload("lobby.txt", "", strings.NewReader("lobby file"), nil); e != nil {
		t.Fatal(e)
	}
	lobby := chatFor(t, bob)
	if len(lobby.Files) != 1 {
		t.Fatal(lobby)
	}
	hostAction(t, admin, "move-user", aliceID, "Reading")
	sendText(t, alice, "reading secret", "")
	if e := alice.upload("reading.txt", "", strings.NewReader("reading file"), nil); e != nil {
		t.Fatal(e)
	}
	for _, r := range []*Remote{alice, bob} {
		d := chatFor(t, r)
		if len(d.Messages) != 1 || len(d.Files) != 1 {
			t.Fatalf("room leak %+v", d)
		}
	}
	response, e := alice.request("GET", fmt.Sprintf("/api/download/%d", lobby.Files[0].ID), nil, "")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode == 200 {
		t.Fatal("cross-room download accepted")
	}
	var search struct {
		Results []Message `json:"results"`
	}
	if e = alice.call("GET", "/api/search?q=lobby", nil, &search); e != nil || len(search.Results) != 0 {
		t.Fatal("search leaked lobby", search, e)
	}
	sendText(t, bob, "cross-room private", fmt.Sprint(aliceID))
	if e = bob.upload("private.txt", fmt.Sprint(aliceID), strings.NewReader("private file"), nil); e != nil {
		t.Fatal(e)
	}
	if d := chatFor(t, alice); len(d.Messages) != 2 || len(d.Files) != 2 {
		t.Fatal("private cross-room missing", d)
	}
	var changed ChatData
	if e = alice.call("GET", "/api/chat-data?room_hint=Lobby&message_after=999999", nil, &changed); e != nil || !changed.RoomChanged || len(changed.Messages) != 2 {
		t.Fatal("room reset", changed, e)
	}
	hostAction(t, admin, "delete-room", 0, "rEaDiNg")
	hostAction(t, admin, "create-room", 0, "Reading")
	hostAction(t, admin, "move-user", aliceID, "Reading")
	d := chatFor(t, alice)
	if len(d.Messages) != 1 || d.Messages[0].Body != "cross-room private" || len(d.Files) != 1 {
		t.Fatal("recreated room exposed archived history", d)
	}
	if e = alice.call("POST", "/api/host/action", map[string]string{"action": "create-room", "room": "Forbidden"}, nil); e == nil {
		t.Fatal("regular user changed rooms")
	}
}
func TestHostInboxAndKickBanRevokeExistingSessions(t *testing.T) {
	s, api, server := testAPI(t)
	admin := adminRemote(t, s, server)
	uid := insertUser(t, s, "alice", "approved")
	alice := userRemote(t, s, server, "alice")
	insertUser(t, s, "bob", "approved")
	bob := userRemote(t, s, server, "bob")
	sendText(t, alice, "@admin Help me please", "")
	if len(chatFor(t, bob).Messages) != 0 {
		t.Fatal("host message broadcast")
	}
	var inbox struct {
		Messages []Message `json:"messages"`
	}
	if e := admin.call("GET", "/api/inbox", nil, &inbox); e != nil || len(inbox.Messages) != 1 || inbox.Messages[0].Body != "Help me please" {
		t.Fatal(inbox, e)
	}
	if e := alice.call("GET", "/api/inbox", nil, &inbox); e == nil {
		t.Fatal("nonadmin read inbox")
	}
	hostAction(t, admin, "kick", uid, "")
	if e := alice.call("GET", "/api/chat-data", nil, nil); e == nil {
		t.Fatal("kicked cookie accepted")
	}
	alice = userRemote(t, s, server, "alice")
	api.mu.Lock()
	api.presence[uid] = Presence{IP: "192.168.1.42"}
	api.mu.Unlock()
	hostAction(t, admin, "ban", uid, "")
	if e := alice.call("GET", "/api/chat-data", nil, nil); e == nil {
		t.Fatal("banned cookie accepted")
	}
	if !api.ipBanned("192.168.1.42") {
		t.Fatal("IP not banned")
	}
	request := httptest.NewRequest("GET", "https://host/api/session", nil)
	request.RemoteAddr = "192.168.1.42:4567"
	request.TLS = &tls.ConnectionState{}
	recorder := httptest.NewRecorder()
	api.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatal("banned IP reached API", recorder.Code)
	}
	if e := alice.call("POST", "/api/login", map[string]string{"username": "alice", "password": testPassword}, nil); e == nil {
		t.Fatal("banned account logged in")
	}
	hostAction(t, admin, "unban", uid, "")
	if api.ipBanned("192.168.1.42") {
		t.Fatal("unban left IP rule")
	}
	if e := alice.call("GET", "/api/chat-data", nil, nil); e == nil {
		t.Fatal("old cookie revived after unban")
	}
	userRemote(t, s, server, "alice")
}
func TestReaderRemembersPinnedHostsAcrossRestartAndForgets(t *testing.T) {
	host, _, remoteServer := testAPI(t)
	a, server := testReader(t)
	if e := a.remember(remoteServer.URL, host.Fingerprint); e != nil {
		t.Fatal(e)
	}
	state := readerAction(t, a, server, ReaderAction{Action: "reconnect", Address: remoteServer.URL})
	if state.Error != "" || state.Stage != "login" || len(state.Recent) != 1 {
		t.Fatalf("reconnect %+v", state)
	}
	state = readerAction(t, a, server, ReaderAction{Action: "login", Username: "admin", Password: testPassword})
	if !state.Connected || state.Error != "" {
		t.Fatalf("login %+v", state)
	}
	state = readerAction(t, a, server, ReaderAction{Action: "create-room", Room: "Books"})
	if state.Error != "" || len(state.Rooms) != 2 {
		t.Fatalf("rooms %+v", state)
	}
	other, e := newApp(a.Store)
	if e != nil {
		t.Fatal(e)
	}
	if len(other.saved) != 1 || other.saved[0].Pin != host.Fingerprint {
		t.Fatal("pinned history not persisted")
	}
	raw, e := os.ReadFile(filepath.Join(a.Store.Data, "trusted-hosts.json"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(testPassword)) {
		t.Fatal("password persisted")
	}
	state = readerAction(t, a, server, ReaderAction{Action: "forget", Address: remoteServer.URL})
	if len(state.Recent) != 0 || state.Error != "" {
		t.Fatal("forget", state)
	}
	other, e = newApp(a.Store)
	if e != nil || len(other.saved) != 0 {
		t.Fatal("forgotten host returned", e)
	}
	state = readerAction(t, a, server, ReaderAction{Action: "reconnect", Address: remoteServer.URL})
	if state.Error == "" {
		t.Fatal("forgotten host reconnected without trust check")
	}
	// The bridge response exposes addresses, never certificate pins or cookies.
	encoded, _ := json.Marshal(state)
	if bytes.Contains(encoded, []byte(host.Fingerprint)) {
		t.Fatal("pin exposed to widgets")
	}
}
