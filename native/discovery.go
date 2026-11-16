package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const discoveryPort = 8002
const discoveryProtocol = "neighbour-chat-discovery/1"

type NearbyHost struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}
type discoveryPacket struct {
	Protocol string `json:"protocol"`
	Kind     string `json:"kind"`
	Nonce    string `json:"nonce"`
	Name     string `json:"name,omitempty"`
	Port     int    `json:"port,omitempty"`
}
type Discovery struct {
	conn *net.UDPConn
	done chan struct{}
	name string
}

func discoveryName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, name)
	if name == "" {
		name = "Kindle chat"
	}
	return truncate(name, 40)
}
func validDiscoveryNonce(nonce string) bool {
	b, e := hex.DecodeString(nonce)
	return e == nil && len(b) == 16
}
func (a *App) discoveryStatus() (bool, string) {
	a.discoveryMu.Lock()
	defer a.discoveryMu.Unlock()
	if a.discovery == nil {
		return false, ""
	}
	select {
	case <-a.discovery.done:
		return false, a.discovery.name
	default:
		return true, a.discovery.name
	}
}

// Caller holds hostMu, which serializes starting/stopping both listeners.
func (a *App) closeDiscovery() {
	a.discoveryMu.Lock()
	defer a.discoveryMu.Unlock()
	if a.discovery != nil {
		a.discovery.conn.Close()
		<-a.discovery.done
		a.discovery = nil
	}
}
func (a *App) setDiscovery(enabled bool, name string) error {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if !enabled {
		a.closeDiscovery()
		return nil
	}
	if a.host == nil {
		return errors.New("Start hosting on this Kindle first.")
	}
	a.closeDiscovery()
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort})
	if e != nil {
		return errors.New("Discovery port 8002 is busy. Discovery is off; your chat is still running.")
	}
	d := &Discovery{conn: conn, done: make(chan struct{}), name: discoveryName(name)}
	a.discoveryMu.Lock()
	a.discovery = d
	a.discoveryMu.Unlock()
	go d.serve(a.API)
	return nil
}
func (d *Discovery) serve(api *API) {
	defer close(d.done)
	defer d.conn.Close()
	var limiter Limiter
	buffer := make([]byte, 513)
	for {
		n, peer, e := d.conn.ReadFromUDP(buffer)
		if n > 512 {
			continue
		} // Windows also reports WSAEMSGSIZE for a truncated datagram.
		if e != nil {
			return
		}
		if peer == nil || (!peer.IP.IsPrivate() && !peer.IP.IsLoopback()) {
			continue
		}
		var query discoveryPacket
		if json.Unmarshal(buffer[:n], &query) != nil || query.Protocol != discoveryProtocol || query.Kind != "find" || !validDiscoveryNonce(query.Nonce) {
			continue
		}
		if !limiter.Allow("all", 80, time.Second) || !limiter.Allow(peer.IP.String(), 6, time.Second) || api.ipBanned(peer.IP.String()) {
			continue
		}
		reply, _ := json.Marshal(discoveryPacket{Protocol: discoveryProtocol, Kind: "host", Nonce: query.Nonce, Name: d.name, Port: 8000})
		d.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		d.conn.WriteToUDP(reply, peer)
	}
}

// One bounded broadcast scan, never a sweep of private subnets or open TCP ports.
func scanNearby(ctx context.Context) ([]NearbyHost, error) {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: discoveryPort}}
	interfaces, e := net.Interfaces()
	if e != nil {
		return nil, e
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, addr := range addresses {
			network, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := network.IP.To4()
			if ip == nil || !ip.IsPrivate() || len(network.Mask) != 4 {
				continue
			}
			broadcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				broadcast[i] = ip[i] | ^network.Mask[i]
			}
			targets = append(targets, &net.UDPAddr{IP: broadcast, Port: discoveryPort})
		}
	}
	return scanTargets(ctx, targets, 3*time.Second)
}
func scanTargets(ctx context.Context, targets []*net.UDPAddr, duration time.Duration) ([]NearbyHost, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{})
	if e != nil {
		return nil, errors.New("Could not scan. Check that Wi-Fi is connected.")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	random := make([]byte, 16)
	if _, e = rand.Read(random); e != nil {
		return nil, e
	}
	nonce := hex.EncodeToString(random)
	packet, _ := json.Marshal(discoveryPacket{Protocol: discoveryProtocol, Kind: "find", Nonce: nonce})
	deadline := time.Now().Add(duration)
	conn.SetDeadline(deadline)
	sent := false
	seenTargets := map[string]bool{}
	for _, target := range targets {
		if seenTargets[target.String()] {
			continue
		}
		seenTargets[target.String()] = true
		if _, e = conn.WriteToUDP(packet, target); e == nil {
			sent = true
		}
	}
	if !sent {
		return nil, errors.New("Wi-Fi discovery could not send a request. You can still use a saved or manual address.")
	}
	result := []NearbyHost{}
	seen := map[string]bool{}
	buffer := make([]byte, 513)
	for len(result) < 20 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			break
		}
		n, peer, err := conn.ReadFromUDP(buffer)
		if n > 512 {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				break
			}
			return nil, err
		}
		if n > 512 || peer == nil || peer.Port != discoveryPort || (!peer.IP.IsPrivate() && !peer.IP.IsLoopback()) {
			continue
		}
		var reply discoveryPacket
		if json.Unmarshal(buffer[:n], &reply) != nil || reply.Protocol != discoveryProtocol || reply.Kind != "host" || reply.Nonce != nonce || reply.Port != 8000 {
			continue
		}
		origin := "https://" + net.JoinHostPort(peer.IP.String(), strconv.Itoa(reply.Port))
		if seen[origin] {
			continue
		}
		seen[origin] = true
		result = append(result, NearbyHost{Name: discoveryName(reply.Name), Address: origin})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Address < result[j].Address
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}
