// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testclient provides in-memory HTTP clients for connection tests.
package testclient

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ConnectionCounter counts connections accepted by a client returned by New.
type ConnectionCounter struct {
	accepted atomic.Int32
}

// Count returns the number of HTTP/1 connections accepted by the test server.
func (c *ConnectionCounter) Count() int32 {
	return c.accepted.Load()
}

// New returns an HTTP client backed by an in-memory server and a counter for
// the connections that server accepts. It avoids consuming TCP source ports,
// so connection-reuse regressions remain testable after port exhaustion.
func New(t *testing.T, handler http.Handler) (*http.Client, *ConnectionCounter) {
	t.Helper()
	listener := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	connections := &ConnectionCounter{}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		select {
		case listener.conns <- serverConn:
			connections.accepted.Add(1)
			return clientConn, nil
		case <-ctx.Done():
			_ = clientConn.Close()
			_ = serverConn.Close()
			return nil, ctx.Err()
		}
	}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return client, connections
}

type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{Port: 1} }
