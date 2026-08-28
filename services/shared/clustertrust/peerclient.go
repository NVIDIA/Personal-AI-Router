// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"bytes"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// PeerIdleTimeout is how long a pooled inter-node connection may sit idle before
// this side reaps it.
const PeerIdleTimeout = 90 * time.Second

// PeerListenerIdleTimeout is the http.Server.IdleTimeout a listener dialed by a
// PeerClientPool should use. It is deliberately LONGER than PeerIdleTimeout
// rather than equal to it: the two ends of a keep-alive must not expire at the
// same instant, or the client can pick a connection the server is already
// closing. Giving the client the shorter lifetime means the side that knows it is
// about to send is always the side that discards a doubtful connection.
const PeerListenerIdleTimeout = PeerIdleTimeout + 15*time.Second

const (
	// peerMaxConnsPerHost caps how many connections we will hold open to one peer
	// at once, including those being dialed. This is the important bound, not the
	// idle one: a per-event fan-out issues one request per active workload per
	// heartbeat, which in the field reached ~300 simultaneous POSTs to a single
	// peer. Left unlimited, everything past the idle pool still opens a fresh
	// connection and pays a fresh mTLS handshake, which is what exhausted the
	// receiver in the first place. Capped, a burst queues on warm connections
	// instead: a warm POST to these endpoints is single-digit milliseconds, so
	// hundreds of queued frames drain well inside the per-request timeout.
	peerMaxConnsPerHost = 8
	// peerMaxIdleConnsPerHost keeps the whole cap warm between bursts, so a
	// steady trickle of events never re-handshakes.
	peerMaxIdleConnsPerHost = peerMaxConnsPerHost
	// peerMaxIdleConns bounds the pool across every peer in a cluster of this
	// scale (~a dozen nodes).
	peerMaxIdleConns = 64
	// peerKeepAlive is the TCP keep-alive probe interval, so a peer that dies
	// without a FIN doesn't leave us holding a black-hole connection.
	peerKeepAlive = 30 * time.Second
)

// PeerClientPool hands out one long-lived *http.Client per pinned cluster peer.
//
// It exists because building an http.Client (and with it an http.Transport) per
// request is not merely wasteful — it is incorrect. A fresh Transport cannot
// reuse a connection, so every request pays a full mTLS handshake, and because a
// hand-built Transport has no IdleConnTimeout its idle connection is never
// reaped and its read loop keeps the Transport reachable, so the socket survives
// for the life of the process. A service that posts per-event to every peer
// therefore leaks one connection per event per peer and eventually cannot
// complete a handshake at all, which presents as dropped inter-node events
// exactly when traffic is heaviest.
//
// The pool is keyed by peer UUID and holds the pinned DER the entry was built
// for, so a peer that re-paired (new leaf, new pin) is rebuilt rather than
// dialed with a stale pin. Cache membership is never itself a trust decision:
// every lookup re-resolves the pin through the configured authority (the Mesh
// by default), so an unpinned peer gets no client at all.
//
// Callers Refresh the mesh on their own cadence, exactly as they must for
// Mesh.ClientTLSConfig; the pool reads current state and does not refresh for
// them.
// PeerClientOptions configures a PeerClientPool. Zero values keep today's
// defaults: Timeout is the whole-request deadline (http.Client.Timeout);
// ResponseHeaderTimeout is unset on the Transport; ResolvePin uses the Mesh.
type PeerClientOptions struct {
	Timeout               time.Duration
	ResponseHeaderTimeout time.Duration
	// ResolvePin overrides the Mesh's disk-backed peer-pin lookup. The resolver
	// is the authorization source and is called without the pool lock held.
	ResolvePin func(peerUUID string) ([]byte, bool)
}

type PeerClientPool struct {
	mesh *Mesh
	opts PeerClientOptions

	mu      sync.Mutex
	entries map[string]*peerClient
}

// peerClient is one peer's pooled client plus BOTH certificates its TLS config
// was frozen around: the peer's pinned DER (what we verify the server against)
// and our own leaf DER (what we present as the client). Either changing
// invalidates the entry — a config built from one identity keeps presenting it
// forever, and a client presenting a superseded leaf is refused by the peer's
// per-request pin gate.
type peerClient struct {
	client    *http.Client
	transport *http.Transport
	pinnedDER []byte
	selfDER   []byte
}

// NewPeerClientPool returns a pool that dials peers of mesh with the given
// per-request timeout (the whole request, as http.Client.Timeout).
func NewPeerClientPool(mesh *Mesh, timeout time.Duration) *PeerClientPool {
	return NewPeerClientPoolOpts(mesh, PeerClientOptions{Timeout: timeout})
}

// NewPeerClientPoolOpts returns a pool with explicit transport deadlines.
// Timeout == 0 leaves http.Client.Timeout unset so a long streaming response
// is not cancelled; dial and handshake still use a finite fallback so a dead
// peer cannot hang the caller. ResponseHeaderTimeout, when set, is applied to
// every pooled Transport.
func NewPeerClientPoolOpts(mesh *Mesh, opts PeerClientOptions) *PeerClientPool {
	return &PeerClientPool{
		mesh:    mesh,
		opts:    opts,
		entries: make(map[string]*peerClient),
	}
}

// Client returns the pooled client for peerUUID. ok is false when this node
// holds no current pin for that peer according to the configured resolver, so a
// caller keeps its existing "skip this peer" path.
// A peer whose pin changed since its entry was built is rebuilt transparently.
func (p *PeerClientPool) Client(peerUUID string) (*http.Client, bool) {
	// Resolve both certificates before taking the pool lock: this is the trust
	// decision, and it must come from the configured authority rather than from
	// cache membership.
	der, pinned := p.resolvePin(peerUUID)
	id, hasIdentity := p.mesh.identity()

	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.entries[peerUUID]; ok {
		if pinned && hasIdentity &&
			bytes.Equal(existing.pinnedDER, der) &&
			bytes.Equal(existing.selfDER, selfLeafDER(id)) {
			return existing.client, true
		}
		// Unpinned now, the peer re-paired, or our own leaf was re-minted (a
		// rejoin or key rotation). In every case the pooled connections were
		// negotiated with a certificate pair that no longer applies, and reusing
		// them would keep presenting a superseded identity until this process
		// restarted.
		existing.transport.CloseIdleConnections()
		delete(p.entries, peerUUID)
	}
	if !pinned || !hasIdentity {
		return nil, false
	}
	self := selfLeafDER(id)
	if self == nil {
		return nil, false
	}

	// Clone both DERs so the entry owns what it compares against. Trust.DER
	// already returns a copy, so that one is belt-and-braces; selfLeafDER hands
	// back the Identity's live slice, so cloning it is the load-bearing half.
	entry := &peerClient{
		transport: p.newTransport(ClientTLSConfig(id.Cert, der)),
		pinnedDER: bytes.Clone(der),
		selfDER:   bytes.Clone(self),
	}
	entry.client = &http.Client{Timeout: p.opts.Timeout, Transport: entry.transport}
	p.entries[peerUUID] = entry
	return entry.client, true
}

func (p *PeerClientPool) resolvePin(peerUUID string) ([]byte, bool) {
	if p.opts.ResolvePin != nil {
		return p.opts.ResolvePin(peerUUID)
	}
	return p.mesh.trustedDER(peerUUID)
}

// selfLeafDER is this node's own leaf certificate in DER form, or nil when the
// identity holds no parsed certificate.
func selfLeafDER(id *Identity) []byte {
	if id == nil || len(id.Cert.Certificate) == 0 {
		return nil
	}
	return id.Cert.Certificate[0]
}

func (p *PeerClientPool) newTransport(tlsCfg *tls.Config) *http.Transport {
	dialTimeout := p.opts.Timeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: peerKeepAlive,
		}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: p.opts.ResponseHeaderTimeout,
		MaxConnsPerHost:       peerMaxConnsPerHost,
		MaxIdleConns:          peerMaxIdleConns,
		MaxIdleConnsPerHost:   peerMaxIdleConnsPerHost,
		IdleConnTimeout:       PeerIdleTimeout,
	}
}

// DropUnpinned closes and forgets every pooled peer this node no longer pins —
// a peer removed from the cluster, or the whole set when this node leaves one.
// It is socket housekeeping, not a gate: Client re-resolves the live pin on every
// call, so an entry surviving here is never what makes a peer reachable.
//
// It resolves all authority and mesh state with the pool lock NOT held, so this
// method takes the same resolver-then-pool order as Client. Holding the pool
// lock while calling a resolver would risk the opposite order if that authority
// ever called back into a pool.
func (p *PeerClientPool) DropUnpinned() {
	p.mu.Lock()
	pooled := make([]string, 0, len(p.entries))
	for uuid := range p.entries {
		pooled = append(pooled, uuid)
	}
	p.mu.Unlock()
	if len(pooled) == 0 {
		return
	}

	id, hasIdentity := p.mesh.identity()
	self := selfLeafDER(id)
	current := make(map[string][]byte, len(pooled))
	for _, uuid := range pooled {
		if der, ok := p.resolvePin(uuid); ok && hasIdentity {
			current[uuid] = der
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, uuid := range pooled {
		entry, ok := p.entries[uuid]
		if !ok {
			continue // already replaced or evicted by a concurrent Client call
		}
		der, pinned := current[uuid]
		if pinned && bytes.Equal(entry.pinnedDER, der) && bytes.Equal(entry.selfDER, self) {
			continue
		}
		entry.transport.CloseIdleConnections()
		delete(p.entries, uuid)
	}
}

// CloseIdle closes every pooled connection and empties the pool. Call it on
// shutdown so a draining process does not leave sockets open.
func (p *PeerClientPool) CloseIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uuid, entry := range p.entries {
		entry.transport.CloseIdleConnections()
		delete(p.entries, uuid)
	}
}

// len reports the number of pooled peers (tests).
func (p *PeerClientPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
