// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/reach"
)

const (
	// perPeerTimeout bounds a single POST so one slow/partitioned peer can't
	// hold up the fan-out (spec §10, §12).
	perPeerTimeout = 3 * time.Second
	// maxBroadcastAttempts is the bounded retry budget per peer. After the
	// last attempt the event is dropped for that peer with a logged final
	// drop (spec §12) — peers reconcile via later events (no replay).
	maxBroadcastAttempts = 3
	// retryBaseDelay is the first backoff interval; it doubles each retry.
	retryBaseDelay = 200 * time.Millisecond
	// maxDrainBytes bounds how much of a peer's response body we read purely to
	// make its connection reusable. Both inter-node answers are an empty 200 or a
	// short error string, so this drains every legitimate body in full.
	maxDrainBytes = 8 << 10
)

// Broadcaster fans a local JSON-RPC notification out to every peer over the
// inter-node interface. Delivery is best-effort: concurrent, per-peer
// timeouts, bounded exponential-backoff retries.
//
// The channel is cluster mTLS, unconditionally: each peer is dialed with a client
// that presents our cluster leaf and pins that peer's exact server cert, and a
// peer we hold no pin for is skipped (the client-side cluster gate). A node that
// belongs to no cluster broadcasts nothing — it does not fall back to posting
// workload events in the clear. Membership is re-derived at the start of every
// round, so a node that joins or leaves mid-session starts (or stops) addressing
// its peers on the next event rather than at its next restart.
//
// Peer clients are pooled and long-lived (clustertrust.PeerClientPool), so a
// fan-out reuses a warm connection per peer instead of handshaking per event.
type Broadcaster struct {
	peers   *peerSet
	mesh    *clustertrust.Mesh
	clients *clustertrust.PeerClientPool
	addrs   *reach.Chooser
}

func NewBroadcaster(peers *peerSet, mesh *clustertrust.Mesh) *Broadcaster {
	return &Broadcaster{
		peers:   peers,
		mesh:    mesh,
		clients: clustertrust.NewPeerClientPool(mesh, perPeerTimeout),
		addrs:   reach.NewChooser(),
	}
}

// CloseIdle releases every pooled peer connection. Called on shutdown.
func (b *Broadcaster) CloseIdle() { b.clients.CloseIdle() }

// DropUnpinned retires pooled clients for peers this node no longer pins.
func (b *Broadcaster) DropUnpinned() { b.clients.DropUnpinned() }

// Broadcast posts the already-marshaled notification frame to every current
// peer concurrently and returns once all peers have either succeeded or
// exhausted their retry budget. ctx cancellation (parent shutdown) aborts
// in-flight attempts.
func (b *Broadcaster) Broadcast(ctx context.Context, frame []byte) {
	// Re-derive membership and pins so a cluster joined, left, or re-paired since
	// the last round is reflected before we fan out, then retire pooled clients
	// for peers those fresh pins no longer cover.
	b.mesh.Refresh()
	b.clients.DropUnpinned()
	if !b.mesh.Clustered() {
		// Not a member: there is no channel to fan out on. Bail before building
		// targets so this is unmistakably a no-op rather than a failed round.
		slog.Debug("broadcast skipped: node is not a cluster member")
		return
	}

	targets := b.peers.targets()
	if len(targets) == 0 {
		slog.Debug("broadcast skipped: no peers")
		return
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			b.deliver(ctx, t, frame)
		}(t)
	}
	wg.Wait()
}

// deliver attempts a single peer with bounded exponential backoff. It resolves
// the peer's pooled pinned client — an unpinned peer is not a trusted member and
// is skipped (the client-side cluster gate) — and reuses it across events. There
// is no unpinned path.
func (b *Broadcaster) deliver(ctx context.Context, t target, frame []byte) {
	delay := retryBaseDelay
	for attempt := 1; attempt <= maxBroadcastAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		// Re-resolve per attempt, not once for the whole budget. The retry window
		// spans seconds, and a peer removed from the cluster inside it must stop
		// being dialed immediately rather than keep receiving our events until the
		// budget runs out.
		client, ok := b.clients.Client(t.uuid)
		if !ok {
			slog.Debug("broadcast skipped: peer not a pinned cluster member", "nodeId", t.id, "uuid", t.uuid)
			return
		}
		hostport := b.addrs.ChooseWithin(ctx, t.id, t.candidates)
		url := "https://" + hostport + eventsPath
		err := b.post(ctx, client, url, frame)
		if err == nil {
			slog.Debug("broadcast delivered", "url", url, "attempt", attempt)
			return
		}
		var statusErr *httpStatusError
		if !errors.As(err, &statusErr) {
			b.addrs.Forget(t.id)
		}
		slog.Warn("broadcast attempt failed",
			"url", url, "attempt", attempt, "max", maxBroadcastAttempts, "err", err)
		if attempt == maxBroadcastAttempts {
			slog.Warn("broadcast dropped after max attempts", "url", url)
			return
		}
		select {
		case <-time.After(delay):
			delay *= 2
		case <-ctx.Done():
			return
		}
	}
}

func (b *Broadcaster) post(ctx context.Context, client *http.Client, url string, frame []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, perPeerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Drain before closing. A body left unread is not merely untidy: the
	// connection cannot be returned to the idle pool, so an error response
	// carrying a body would silently cost us the pooled connection and put the
	// next event back to a full handshake. Bounded, because this endpoint only
	// ever answers with an empty 200 or a short error string — a peer streaming
	// something larger forfeits its connection instead of our bandwidth.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{code: resp.StatusCode, url: url}
	}
	return nil
}

type httpStatusError struct {
	code int
	url  string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("peer returned HTTP %d", e.code)
}
