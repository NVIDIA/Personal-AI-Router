// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"nvpair-shared/clustertrust"
)

// eventsPath is the single inter-node endpoint (spec §7.2).
const eventsPath = "/v1/workloads/events"

// interNodeReadHeaderTimeout bounds how long a peer may take to send its request
// headers.
const interNodeReadHeaderTimeout = 5 * time.Second

// newInterNodeServer builds the inter-node HTTP server.
//
// IdleTimeout is the half of connection reuse that lives on the listener.
// Without it every connection a peer opens pins a file descriptor here until the
// process exits, which is how a busy cluster runs itself out of the ability to
// accept a handshake at all. It uses clustertrust.PeerListenerIdleTimeout, which
// is deliberately longer than the calling pool's own idle lifetime so the client
// always reaps first and never picks a connection this side is closing.
func newInterNodeServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: interNodeReadHeaderTimeout,
		IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
	}
}

// Server is the inter-node HTTP listener. Peers POST JSON-RPC notifications
// here; each validated, deduplicated event is translated and emitted to the
// local broker on stdout.
type Server struct {
	port  int
	dedup *dedupIndex

	// emitUpsert / emitRemove forward a translated event to the broker. They
	// wrap the codec, whose writes are serialized so frames never interleave.
	emitUpsert func(w *Workload) error
	emitRemove func(workloadID, nodeID string) error

	// mesh is this node's live cluster state. The inter-node interface is cluster
	// mTLS unconditionally (§7.2); the mesh decides per request WHICH callers are
	// pinned members, and per handshake whether this node can serve at all — never
	// once at bind time, because a node joins and leaves while this process runs.
	mesh *clustertrust.Mesh

	srv *http.Server
}

func NewServer(port int, dedup *dedupIndex, mesh *clustertrust.Mesh, emitUpsert func(*Workload) error, emitRemove func(workloadID, nodeID string) error) *Server {
	return &Server{
		port:       port,
		dedup:      dedup,
		mesh:       mesh,
		emitUpsert: emitUpsert,
		emitRemove: emitRemove,
	}
}

// Run binds the inter-node port once and serves cluster mTLS on it for the life
// of the process. There is no plain-HTTP personality: this is a cluster data
// plane, so a node that belongs to no cluster serves nothing here.
//
// The server certificate is resolved per handshake from the live mesh, which is
// what lets one listener cover both states — while unclustered it presents no
// leaf and every handshake is refused, and the moment this node becomes a member
// the same listener serves pinned peers. No rebind, and no restart to re-read the
// cluster dir. Blocks until ctx is cancelled, then drains.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, s.handleEvents)

	base, err := net.Listen("tcp", ":"+strconv.Itoa(s.port))
	if err != nil {
		return fmt.Errorf("listen on :%d: %w", s.port, err)
	}

	s.srv = newInterNodeServer(mux)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("inter-node server listening (cluster mTLS)",
			"addr", base.Addr().String(), "clustered", s.mesh.Clustered())
		if serveErr := s.srv.Serve(tls.NewListener(base, s.mesh.ServerTLSConfig())); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Cluster gate: the event must arrive over cluster mTLS from a peer this node
	// currently pins. Refresh first so a membership change or a peer paired since
	// the last request is reflected without a restart.
	//
	// Unconditional by design. VerifyClientPin is already false for a node that
	// belongs to no cluster, so an unclustered node rejects every event — it does
	// not fall back to accepting them in the clear. Wrapping this in a membership
	// test is what made an unclustered node an open ingest endpoint (§7.2).
	s.mesh.Refresh()
	if _, ok := s.mesh.VerifyClientPin(r); !ok {
		http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.badRequest(w, "read body: "+err.Error())
		return
	}

	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		s.badRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if msg.JSONRPC != "2.0" {
		s.badRequest(w, "unsupported jsonrpc version")
		return
	}

	switch {
	case isLifecycleMethod(msg.Method):
		s.handleLifecycle(w, &msg)
	case msg.Method == MethodRemove:
		s.handleRemove(w, &msg)
	default:
		s.badRequest(w, "unknown method: "+msg.Method)
	}
}

func (s *Server) handleLifecycle(w http.ResponseWriter, msg *Message) {
	wl, err := parseLifecycle(msg.Params)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	// Deduplicate by identity + state. A re-broadcast with the same key is
	// accepted (200 OK) but not re-emitted to the broker, so the catalog updates
	// at most once — EXCEPT for a re-sync frame (anti-entropy heartbeat /
	// discovery backfill), which intentionally re-asserts the same key and must
	// reach the broker so its store can reconcile (e.g. un-stick a wrongly
	// inferred failed). The store is idempotent, so bypassing dedup here is safe.
	if !isResyncFrame(msg.Params) && s.dedup.seenOrAdd(keyLifecycle(wl)) {
		slog.Debug("inter-node lifecycle deduplicated", "method", msg.Method, "id", wl.ID, "state", wl.State)
		s.ok(w)
		return
	}

	if err := s.emitUpsert(wl); err != nil {
		// A failed stdout write means the broker is gone; report a server
		// error so the peer's retry budget can kick in, but the local
		// interface severing is handled as a shutdown signal elsewhere.
		slog.Error("failed to emit workloads:upsert", "id", wl.ID, "err", err)
		http.Error(w, "broker unavailable", http.StatusInternalServerError)
		return
	}
	slog.Info("relayed remote lifecycle as upsert", "method", msg.Method, "id", wl.ID, "state", wl.State, "node", wl.OriginatedFrom)
	s.ok(w)
}

// isResyncFrame reports whether a lifecycle notification is an anti-entropy
// re-assertion (params carry "resync": true), which must bypass the state dedup.
func isResyncFrame(params json.RawMessage) bool {
	var p struct {
		Resync bool `json:"resync"`
	}
	return json.Unmarshal(params, &p) == nil && p.Resync
}

func (s *Server) handleRemove(w http.ResponseWriter, msg *Message) {
	workloadID, nodeID, err := parseRemove(msg.Params)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	if s.dedup.seenOrAdd(keyRemove(nodeID, workloadID)) {
		slog.Debug("inter-node removal deduplicated", "workloadId", workloadID, "node", nodeID)
		s.ok(w)
		return
	}

	if err := s.emitRemove(workloadID, nodeID); err != nil {
		slog.Error("failed to emit workloads:remove", "workloadId", workloadID, "node", nodeID, "err", err)
		http.Error(w, "broker unavailable", http.StatusInternalServerError)
		return
	}
	slog.Info("relayed remote removal", "workloadId", workloadID, "node", nodeID)
	s.ok(w)
}

func (s *Server) ok(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) badRequest(w http.ResponseWriter, reason string) {
	slog.Warn("inter-node request rejected", "status", http.StatusBadRequest, "reason", reason)
	http.Error(w, reason, http.StatusBadRequest)
}
