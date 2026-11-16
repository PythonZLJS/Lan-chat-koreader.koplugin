package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func discoveryTarget() *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: discoveryPort}
}
func TestDiscoveryOnOffKeepsHostAndSessionsAlive(t *testing.T) {
	a, server := testReader(t)
	state := readerAction(t, a, server, ReaderAction{Action: "host"})
	if state.Error != "" || !state.Admin || !state.Connected || state.Discoverable {
		t.Fatalf("owner not automatically admin or discovery not off: %+v", state)
	}
	state = readerAction(t, a, server, ReaderAction{Action: "discovery-on", Name: "Reading group"})
	if state.Error != "" || !state.Discoverable {
		t.Fatalf("enable: %+v", state)
	}
	// Exercise an actual pinned HTTPS client as well as the local owner session.
	insertUser(t, a.Store, "nearbyreader", "approved")
	peer := newRemote("https://127.0.0.1:8000", a.Store.Fingerprint)
	if _, e := peer.info(); e != nil {
		t.Fatal(e)
	}
	if e := peer.call("POST", "/api/login", map[string]string{"username": "nearbyreader", "password": testPassword}, nil); e != nil {
		t.Fatal(e)
	}
	// Oversized/invalid UDP queries must not kill the responder.
	noise, e := net.DialUDP("udp4", nil, discoveryTarget())
	if e != nil {
		t.Fatal(e)
	}
	noise.Write([]byte(strings.Repeat("x", 800)))
	noise.Write([]byte(`{"protocol":"neighbour-chat-discovery/1","kind":"find","nonce":"invalid"}`))
	noise.Close()
	targets := []*net.UDPAddr{discoveryTarget()}
	found, e := scanTargets(context.Background(), targets, 150*time.Millisecond)
	if e != nil || len(found) != 1 || found[0].Name != "Reading group" || found[0].Address != "https://127.0.0.1:8000" {
		t.Fatal("discovery", found, e)
	}
	// Closing the discovery listener cannot sign clients out or stop TLS hosting.
	state = readerAction(t, a, server, ReaderAction{Action: "discovery-off"})
	if state.Discoverable || !state.Hosting || !state.Connected || !state.Admin {
		t.Fatalf("off disrupted chat: %+v", state)
	}
	if _, e = a.reader.remote.info(); e != nil {
		t.Fatal("chat stopped with discovery", e)
	}
	info, e := peer.info()
	if e != nil || !info.Authenticated || info.Admin {
		t.Fatal("discovery off disrupted regular client's TLS session or elevated its role", info, e)
	}
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort})
	if e != nil {
		t.Fatal("discovery socket was not released", e)
	}
	conn.Close()
	readerAction(t, a, server, ReaderAction{Action: "discovery-on", Name: "Again"})
	readerAction(t, a, server, ReaderAction{Action: "stop-host"})
	if on, _ := a.discoveryStatus(); on {
		t.Fatal("discovery survived host shutdown")
	}
	if e = a.setDiscovery(true, "Not hosting"); e == nil {
		t.Fatal("discovery allowed without server")
	}
}
func TestDiscoveryRejectsUnrelatedForgedAndOversizedReplies(t *testing.T) {
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: discoveryPort})
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 512)
		n, peer, err := conn.ReadFromUDP(b)
		if err != nil {
			return
		}
		var query discoveryPacket
		json.Unmarshal(b[:n], &query)
		for _, p := range []discoveryPacket{
			{Protocol: "unrelated", Kind: "host", Nonce: query.Nonce, Name: "Wrong protocol", Port: 8000},
			{Protocol: discoveryProtocol, Kind: "host", Nonce: strings.Repeat("0", 32), Name: "Wrong scan", Port: 8000},
			{Protocol: discoveryProtocol, Kind: "host", Nonce: query.Nonce, Name: "Wrong port", Port: 22},
			{Protocol: discoveryProtocol, Kind: "host", Nonce: query.Nonce, Name: strings.Repeat("x", 600), Port: 8000},
			{Protocol: discoveryProtocol, Kind: "host", Nonce: query.Nonce, Name: "Valid\nname", Port: 8000},
		} {
			raw, _ := json.Marshal(p)
			conn.WriteToUDP(raw, peer)
		}
	}()
	found, e := scanTargets(context.Background(), []*net.UDPAddr{discoveryTarget()}, 150*time.Millisecond)
	<-done
	if e != nil || len(found) != 1 || found[0].Name != "Validname" {
		t.Fatal("forged reply accepted", found, e)
	}
}
func TestDiscoveryScanCancellationAndNoPublicOwnerBypass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, e := scanTargets(ctx, []*net.UDPAddr{discoveryTarget()}, 3*time.Second)
	if e == nil || time.Since(started) > time.Second {
		t.Fatal("cancel did not stop scan", e)
	}
	s, _, server := testAPI(t)
	guest := newRemote(server.URL, s.Fingerprint)
	if _, e = guest.info(); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/owner", "/api/discovery-on", "/api/host-owner"} {
		if e = guest.call("POST", path, map[string]string{}, nil); e == nil {
			t.Fatal("public owner bypass", path)
		}
	}
}
func TestReaderScanSelectionRequiresResultAndStillVerifiesTLS(t *testing.T) {
	a, server := testReader(t)
	state := readerAction(t, a, server, ReaderAction{Action: "join-nearby", Address: "https://127.0.0.1:8000"})
	if state.Error == "" {
		t.Fatal("arbitrary address accepted as scan result")
	}
	readerAction(t, a, server, ReaderAction{Action: "host"})
	state = a.reader.snapshot()
	if !state.Admin || state.Address != "This Kindle" {
		t.Fatal("host is not owner admin", state)
	}
	// Joining a discovered host still goes through certificate probing/check code.
	a.reader.change(func(s *ReaderState) { s.Nearby = []NearbyHost{{Name: "Local test", Address: "https://127.0.0.1:8000"}} })
	state = readerAction(t, a, server, ReaderAction{Action: "join-nearby", Address: "https://127.0.0.1:8000"})
	if state.Stage != "verify" || state.Connected || state.Code == "" || state.Error != "" {
		t.Fatalf("discovery skipped certificate check: %+v", state)
	}
}
