package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountSwitchDropsPrivateCachesAndRejectsOldActions(t *testing.T) {
	a, server := testReader(t)
	initial := readerAction(t, a, server, ReaderAction{Action: "host"})
	alice := insertUser(t, a.Store, "alice", "approved")
	bob := insertUser(t, a.Store, "bob", "approved")
	readerAction(t, a, server, ReaderAction{Action: "send", Body: "Only host and Bob", Recipient: fmt.Sprint(bob), Token: strings.Repeat("b", 24)})
	readerAction(t, a, server, ReaderAction{Action: "send", Body: "Public chat", Token: strings.Repeat("p", 24)})
	a.reader.change(func(s *ReaderState) {
		s.Inbox = []Message{{Body: "Private inbox"}}
		s.Results = []Message{{Body: "Private search"}}
		s.Accounts = []User{{ID: bob}}
	})
	next := readerAction(t, a, server, ReaderAction{Action: "login", Username: "alice", Password: testPassword})
	if next.Error != "" || !next.Connected || next.Admin || next.UserID != alice || len(next.Messages) != 1 || next.Messages[0].Body != "Public chat" || len(next.Inbox) != 0 || len(next.Results) != 0 || len(next.Accounts) != 0 || next.Context == initial.Context {
		t.Fatalf("account switch retained private state: %+v", next)
	}
	stale := readerAction(t, a, server, ReaderAction{Action: "send", Context: initial.Context, Body: "Stale editor", Token: strings.Repeat("s", 24)})
	if stale.Error == "" {
		t.Fatal("stale context accepted")
	}
	var count int
	if e := a.Store.DB.QueryRow("SELECT count(*) FROM messages WHERE body='Stale editor'").Scan(&count); e != nil || count != 0 {
		t.Fatal("stale message stored", e, count)
	}
}
func TestRoomHintRejectsMessagesAndFilesAfterUnseenRoomMove(t *testing.T) {
	s, _, server := testAPI(t)
	admin := adminRemote(t, s, server)
	aliceID := insertUser(t, s, "alice", "approved")
	alice := userRemote(t, s, server, "alice")
	hostAction(t, admin, "create-room", 0, "Books")
	hostAction(t, admin, "move-user", aliceID, "Books")
	e := alice.call("POST", "/api/send", map[string]string{"body": "Meant for Lobby", "room_hint": "Lobby"}, nil)
	if e == nil {
		t.Fatal("message silently followed an unseen room move")
	}
	if e = alice.upload("private-note.txt", "", strings.NewReader("Meant for Lobby"), nil, "Lobby"); e == nil {
		t.Fatal("file silently followed an unseen room move")
	}
	d := chatFor(t, alice)
	if len(d.Messages) != 0 || len(d.Files) != 0 {
		t.Fatal("rejected content retained", d)
	}
}
func TestOverlappingIPBansAndLegacyBanMigration(t *testing.T) {
	s, api, server := testAPI(t)
	admin := adminRemote(t, s, server)
	alice := insertUser(t, s, "alice", "approved")
	bob := insertUser(t, s, "bob", "approved")
	// Reproduce the v7 one-IP/one-user schema and verify lossless migration.
	if _, e := s.DB.Exec(`DROP TABLE banned_ips;CREATE TABLE banned_ips(ip TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE);INSERT INTO banned_ips(ip,user_id)VALUES('192.168.1.44',?)`, alice); e != nil {
		t.Fatal(e)
	}
	if e := s.migrateIPBans(); e != nil {
		t.Fatal(e)
	}
	if e := s.migrateIPBans(); e != nil {
		t.Fatal("migration not idempotent", e)
	}
	if !api.ipBanned("192.168.1.44") {
		t.Fatal("legacy IP rule lost")
	}
	api.mu.Lock()
	api.presence = map[int64]Presence{alice: {IP: "192.168.1.44"}, bob: {IP: "192.168.1.44"}}
	api.mu.Unlock()
	hostAction(t, admin, "ban", alice, "")
	hostAction(t, admin, "ban", bob, "")
	hostAction(t, admin, "unban", bob, "")
	if !api.ipBanned("192.168.1.44") {
		t.Fatal("unbanning Bob removed Alice's IP ban")
	}
	hostAction(t, admin, "unban", alice, "")
	if api.ipBanned("192.168.1.44") {
		t.Fatal("last IP ban did not clear")
	}
}
func TestConcurrentSessionRotationCannotForkOneCookie(t *testing.T) {
	old := APISession{ID: randomToken(32), CSRF: randomToken(32), Expires: time.Now().Add(time.Hour)}
	sessions := APISessions{entries: map[string]APISession{old.ID: old}}
	var accepted atomic.Int32
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if _, ok := sessions.rotate(httptest.NewRecorder(), old, 2, false, 0); ok {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if accepted.Load() != 1 || len(sessions.entries) != 1 {
		t.Fatal("one cookie forked into sessions", accepted.Load(), len(sessions.entries))
	}
}
func TestLocalOwnerCanRecoverUnderFullSessionTable(t *testing.T) {
	a, _ := testReader(t)
	a.API.Sessions.entries = make(map[string]APISession)
	for i := 0; i < 512; i++ {
		id := fmt.Sprint(i)
		a.API.Sessions.entries[id] = APISession{ID: id, UID: 2, Expires: time.Now().Add(time.Hour)}
	}
	for i := 0; i < 30; i++ {
		owner, e := a.ownerRemote()
		if e != nil {
			t.Fatal("session churn blocked local owner", e)
		}
		info, e := owner.info()
		if e != nil || !info.Authenticated || !info.Admin {
			t.Fatal("owner could not recover", info, e)
		}
		if len(a.API.Sessions.entries) != 513 {
			t.Fatal("reserved owner slot leaked", len(a.API.Sessions.entries))
		}
	}
}
func TestDiscoverySocketFailureIsReportedAndCanRestart(t *testing.T) {
	a, server := testReader(t)
	readerAction(t, a, server, ReaderAction{Action: "host"})
	if e := a.setDiscovery(true, "Before"); e != nil {
		t.Fatal(e)
	}
	a.discoveryMu.Lock()
	d := a.discovery
	a.discoveryMu.Unlock()
	d.conn.Close()
	<-d.done
	if on, _ := a.discoveryStatus(); on {
		t.Fatal("dead listener reported discoverable")
	}
	if e := a.setDiscovery(true, "After"); e != nil {
		t.Fatal("listener could not restart", e)
	}
	found, e := scanTargets(a.reader.ctx, []*net.UDPAddr{discoveryTarget()}, 150*time.Millisecond)
	if e != nil || len(found) != 1 || found[0].Name != "After" {
		t.Fatal(found, e)
	}
}

func TestLargeCombinedReaderCachesStayWithinLuaResponseLimit(t *testing.T) {
	a, server := testReader(t)
	large := strings.Repeat("界", 4000)
	a.reader.change(func(s *ReaderState) {
		for i := int64(0); i < 100; i++ {
			s.Results = append(s.Results, Message{ID: i, Body: large})
		}
		for i := int64(0); i < 50; i++ {
			s.Inbox = append(s.Inbox, Message{ID: i, Body: large})
		}
		for i := int64(12); i > 0; i-- {
			s.Messages = append(s.Messages, Message{ID: i, Body: large})
		}
		for i := int64(0); i < 500; i++ {
			u := User{ID: i, Display: strings.Repeat("界", 40), Username: strings.Repeat("a", 32), Room: strings.Repeat("界", 64)}
			s.Accounts = append(s.Accounts, u)
			s.Users = append(s.Users, u)
		}
	})
	response := readerRequest(t, a, server, nil)
	defer response.Body.Close()
	var got ReaderState
	if response.StatusCode != 200 {
		t.Fatal("large cache broke bridge", response.StatusCode)
	}
	if e := json.NewDecoder(response.Body).Decode(&got); e != nil {
		t.Fatal(e)
	}
	payload, e := json.Marshal(got)
	if e != nil || len(payload) > 480<<10 || got.Notice == "" || len(got.Results) == 0 {
		t.Fatal("state not bounded", len(payload), e)
	}
	// Preview limits must never mutate or delete server-side caches/content.
	if len(a.reader.snapshot().Results) != 100 || len(a.reader.snapshot().Messages) != 12 {
		t.Fatal("state preview mutated the source")
	}
}
