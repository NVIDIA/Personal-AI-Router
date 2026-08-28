// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package splitlisten fans the connections accepted from a single base listener
// onto two virtual net.Listeners chosen by each connection's first byte: a TLS
// handshake record (0x16) goes to TLS(), anything else — an ASCII HTTP method
// byte — goes to Plain(). The peeked byte is restored before the connection is
// handed on, so the receiving server reads the whole stream unchanged.
//
// This is the single-sourced form of the first-byte transport split
// nvpair-cluster-manager pioneered (a plain-HTTP pairing channel and mTLS
// trusted endpoints sharing one port, distinguished by the connection's first
// byte). It carries no policy of its own — the caller decides what to serve on
// each sub-listener (e.g. wrap TLS() with tls.NewListener, gate Plain() to
// loopback) — so the promoted proxies and cluster-manager share one dispatcher
// instead of copying it.
package splitlisten

import (
	"log/slog"
	"net"
	"sync"
	"time"
)

// tlsHandshakeByte is the first byte of a TLS record with content-type
// handshake (22 / 0x16). Every TLS ClientHello starts with it, and no ASCII
// HTTP method does, so it cleanly separates the two personalities.
const tlsHandshakeByte = 0x16

// peekTimeout bounds how long dispatch waits for a connection's first byte
// before discarding it, so a silent or half-open client can't pin a dispatch
// goroutine indefinitely. A var (not const) so tests can shorten it.
var peekTimeout = 5 * time.Second

// pushTimeout bounds how long dispatch waits for a sub-listener Accept to take
// the connection. The handoff channel is unbuffered, so the timeout covers the
// full wait until Accept — not merely enqueue into a backlog. If the
// corresponding http.Server has stopped Accepting, we close the conn instead of
// pinning an FD forever. A var so tests can shorten it.
var pushTimeout = 5 * time.Second

// Splitter dispatches connections from a base listener to a plain and a TLS
// sub-listener by first byte. Construct with New; retrieve the sub-listeners
// with Plain and TLS; stop everything with Close (or by closing the base
// listener out from under it).
type Splitter struct {
	base  net.Listener
	plain *chanListener
	tls   *chanListener
}

// New wraps base and starts the accept+dispatch loop in a goroutine. The
// returned Splitter owns base: Close closes it, and an Accept error on base
// (e.g. base closed elsewhere) tears the Splitter down.
func New(base net.Listener) *Splitter {
	s := &Splitter{
		base:  base,
		plain: newChanListener(base.Addr()),
		tls:   newChanListener(base.Addr()),
	}
	go s.run()
	return s
}

// Plain returns the sub-listener that receives non-TLS (plain HTTP)
// connections.
func (s *Splitter) Plain() net.Listener { return s.plain }

// TLS returns the sub-listener that receives TLS connections. Wrap it with
// tls.NewListener to terminate TLS.
func (s *Splitter) TLS() net.Listener { return s.tls }

// Addr reports the base listener's address (shared by both personalities).
func (s *Splitter) Addr() net.Addr { return s.base.Addr() }

// Close stops dispatching and closes the base and both sub-listeners. Servers
// running on the sub-listeners see their Accept return net.ErrClosed and
// unwind. Idempotent.
func (s *Splitter) Close() error {
	err := s.base.Close()
	s.plain.Close()
	s.tls.Close()
	return err
}

// run accepts from the base listener until a non-temporary Accept error,
// dispatching each connection on its own goroutine so a slow first-byte read
// never blocks the accept loop. Temporary Accept errors are retried with
// backoff (same shape as http.Server.Serve). A fatal Accept error closes the
// base listener as well as the sub-listeners so the socket cannot sit forever
// in LISTEN with nobody Accepting (macOS backlog exhaustion).
func (s *Splitter) run() {
	var tempDelay time.Duration
	for {
		conn, err := s.base.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := time.Second; tempDelay > max {
					tempDelay = max
				}
				time.Sleep(tempDelay)
				continue
			}
			slog.Error("splitlisten accept stopped", "err", err, "addr", s.base.Addr().String())
			_ = s.base.Close()
			s.plain.Close()
			s.tls.Close()
			return
		}
		tempDelay = 0
		go s.dispatch(conn)
	}
}

// dispatch peeks the first byte (under peekTimeout), restores it via prefixConn,
// and routes the connection to the TLS sub-listener for a handshake record or
// the plain sub-listener otherwise. A read error or empty read drops the
// connection.
func (s *Splitter) dispatch(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout))
	b := make([]byte, 1)
	n, err := conn.Read(b)
	if err != nil || n == 0 {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	pc := &prefixConn{Conn: conn, prefix: b[:n]}
	if b[0] == tlsHandshakeByte {
		s.tls.push(pc)
	} else {
		s.plain.push(pc)
	}
}

// prefixConn re-yields the bytes peeked off the wire before delegating further
// reads to the underlying connection, so the server that receives it sees the
// original byte stream from the start.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// chanListener is a net.Listener fed connections from the dispatcher rather than
// accepting them itself. Accept blocks until a connection is pushed or the
// listener is closed.
type chanListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
	pushMu sync.Mutex
	pushes sync.WaitGroup
}

func newChanListener(addr net.Addr) *chanListener {
	// Unbuffered: push blocks until Accept takes the conn, so pushTimeout
	// bounds the wait until handoff rather than until enqueue into a backlog.
	return &chanListener{addr: addr, conns: make(chan net.Conn), closed: make(chan struct{})}
}

// push hands a connection to a blocked Accept, or closes it if the listener has
// already shut down or no Accept takes it within pushTimeout (so a dead
// http.Server cannot pin accepted FDs forever).
func (l *chanListener) push(c net.Conn) {
	l.pushMu.Lock()
	select {
	case <-l.closed:
		l.pushMu.Unlock()
		_ = c.Close()
		return
	default:
		l.pushes.Add(1)
		l.pushMu.Unlock()
	}
	defer l.pushes.Done()

	timer := time.NewTimer(pushTimeout)
	defer timer.Stop()

	// Prefer closed over send when both are already ready so a push racing
	// Close closes the conn rather than completing handoff into teardown.
	select {
	case <-l.closed:
		_ = c.Close()
		return
	default:
	}
	select {
	case <-l.closed:
		_ = c.Close()
	case <-timer.C:
		_ = c.Close()
	case l.conns <- c:
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() {
		l.pushMu.Lock()
		close(l.closed)
		l.pushMu.Unlock()

		// Wait until pushes that started before closure have either handed off
		// or closed their connection. No push can register after closed is
		// signaled. Handoff is unbuffered, so a completed send already reached
		// Accept; drain is only a belt-and-suspenders reclaim.
		l.pushes.Wait()
		for {
			select {
			case c := <-l.conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
