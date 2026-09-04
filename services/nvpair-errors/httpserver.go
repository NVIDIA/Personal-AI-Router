// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// httpserver.go is the inbound surface for cross-node error sync.
//
// It is cluster mTLS, unconditionally. This is a cluster data plane, not a LAN
// service: the caller must present a certificate this node currently pins, and
// there is no plain-HTTP personality for a node that belongs to no cluster —
// such a node serves nothing here. The trust boundary is cluster membership, not
// the local network.
//
//	POST /v1/errors  -> ingest a peer's full local-origin snapshot
//	                    (SyncEnvelope) and reconcile it into the store.
//	GET  /v1/errors  -> return THIS node's local-origin snapshot; the
//	                    same body peers receive when we push to them.
//
// The push side (outbound POSTs to peers) lives in peersync.go.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
)

// maxIngestBodyBytes bounds an inbound error-sync snapshot, matching the
// 1 MiB cap the other inter-node endpoints apply to request bodies.
const maxIngestBodyBytes = 1 << 20

// newErrorsMux builds the HTTP routes backed by the manager. Split out
// from the listener so tests can exercise the handlers via
// httptest.NewServer without binding a real port or touching mDNS.
func newErrorsMux(mgr *Manager, mesh *clustertrust.Mesh) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/errors", func(w http.ResponseWriter, r *http.Request) {
		// The caller must be a pinned cluster peer. The TLS layer accepted any
		// client cert (RequireAnyClientCert); this is where a non-member is turned
		// away with a real 403 rather than an opaque handshake failure. Refresh
		// first so a cluster joined or left while this process runs is reflected
		// here rather than at the next restart.
		//
		// The check is deliberately unconditional — not "if this node is
		// clustered". VerifyClientPin already answers false for a node that
		// belongs to no cluster, so an unclustered node rejects everything, which
		// is the intended posture. Wrapping it in a membership test is what let an
		// unclustered node serve this data in the clear, so there is no branch here
		// for a future caller to widen.
		mesh.Refresh()
		callerUUID, ok := mesh.VerifyClientPin(r)
		if !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPost:
			handleIngest(w, r, mgr, callerUUID)
		case http.MethodGet:
			handleServeLocal(w, mgr)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

// handleIngest reconciles a peer's pushed SyncEnvelope. The envelope's nodeId
// is NOT trusted: the origin identity is the mTLS-authenticated caller UUID,
// which the pin store keys identically to the nodeId the push side uses. A
// mismatched body value is a 400 (caught by tests and honest peers), a
// malformed body is a 400, and the manager still rejects reconciling our own
// origin as a no-op.
func handleIngest(w http.ResponseWriter, r *http.Request, mgr *Manager, callerUUID string) {
	defer r.Body.Close()
	var env errors.SyncEnvelope
	if err := json.NewDecoder(io.LimitReader(r.Body, maxIngestBodyBytes)).Decode(&env); err != nil {
		slog.Warn("ingest: bad request body", "err", err)
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if env.NodeID == "" {
		http.Error(w, `"nodeId" is required`, http.StatusBadRequest)
		return
	}
	if env.NodeID != callerUUID {
		slog.Warn("ingest: envelope nodeId does not match the authenticated caller",
			"caller", callerUUID)
		http.Error(w, "nodeId does not match the authenticated caller", http.StatusBadRequest)
		return
	}

	mgr.ReconcilePeer(callerUUID, env.Errors)
	slog.Debug("ingested peer snapshot", "nodeId", callerUUID, "count", len(env.Errors))
	w.WriteHeader(http.StatusNoContent)
}

// handleServeLocal returns this node's local-origin errors as a
// SyncEnvelope — the exact shape we push to peers, so the GET endpoint
// doubles as a self-describing probe of what this node would propagate.
func handleServeLocal(w http.ResponseWriter, mgr *Manager) {
	env := errors.SyncEnvelope{
		NodeID: mgr.LocalNodeID(),
		Errors: mgr.LocalSnapshot(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		slog.Warn("serve-local: encode failed", "err", err)
	}
}

// runHTTPServer binds the ingest/serve endpoints on the given port and serves
// until ctx is cancelled, then drains with a short grace period. A bind failure
// is returned so the caller can decide whether it's fatal (it is in main.go: no
// inbound endpoint means peers can't reach us).
//
// The listener is cluster mTLS only — there is no plain personality to fall back
// to. Its certificate is resolved per handshake from the live mesh, which is what
// lets the port stay bound for the life of the process: while this node belongs
// to no cluster it presents no leaf and every handshake is refused, and the
// moment it becomes a member the same listener serves pinned peers. Nothing has
// to be rebound, and no restart is needed to re-read the cluster dir.
func runHTTPServer(ctx context.Context, port int, mgr *Manager, mesh *clustertrust.Mesh) error {
	base, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", port, err)
	}

	// Without an IdleTimeout a peer that has forgotten a connection leaves us
	// pinning its descriptor for the life of the process. The listener's lifetime
	// is deliberately longer than the calling pool's, so the client always reaps
	// first and never picks a connection this side is closing.
	server := &http.Server{
		Handler:           newErrorsMux(mgr, mesh),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("errors inter-node server listening (cluster mTLS)",
			"port", port, "clustered", mesh.Clustered())
		if serveErr := server.Serve(tls.NewListener(base, mesh.ServerTLSConfig())); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case serveErr := <-errCh:
		return serveErr
	}
}
