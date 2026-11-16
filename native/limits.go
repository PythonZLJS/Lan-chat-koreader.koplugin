package main

import (
	"net"
	"sync"
)

type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}
type boundedConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *boundedConn) Close() error { e := c.Conn.Close(); c.once.Do(func() { <-c.slots }); return e }
func limitConnections(listener net.Listener, n int) net.Listener {
	return &boundedListener{Listener: listener, slots: make(chan struct{}, n), done: make(chan struct{})}
}
func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	c, e := l.Listener.Accept()
	if e != nil {
		<-l.slots
		return nil, e
	}
	return &boundedConn{Conn: c, slots: l.slots}, nil
}
func (l *boundedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}
