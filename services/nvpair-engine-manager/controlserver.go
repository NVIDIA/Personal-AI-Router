// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// controlserver.go is engine-manager's cluster-scoped remote-control surface —
// the "ec" service. Unlike the plain /v1/models endpoint (em, model names are
// not secret), this surface performs privileged operations on behalf of a
// remote node (remote install/pull/start/stop + engine status), so it is
// pin-based mTLS and admits a caller ONLY while this node is a cluster member.
// Any caller that isn't a byte-for-byte pinned cluster peer is turned away with
// a real HTTP 403 rather than an opaque handshake failure — the same decision
// nvpair-errors and nvpair-workload-manager make via nvpair-shared/clustertrust.
//
// The listener is bound whenever the parent opts in with --control-port, and its
// TLS identity is resolved per handshake from the live mesh: while this node is
// not a member there is no leaf to present, so the handshake is refused and
// nothing privileged is reachable. Binding on membership instead would make this
// surface unavailable for the life of the process to a node that joined a cluster
// after engine-manager started.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"nvpair-shared/clustertrust"
)

// controlEnginesPath returns this node's engine install/run status.
const controlEnginesPath = "/v1/engines"

// controlServer holds the dependencies the ec handlers share.
type controlServer struct {
	exec *Executor
	mesh *clustertrust.Mesh
}

// requirePin gates a handler on cluster-peer mTLS, sharing the one gate the
// model surface uses (requirePinnedPeer in httpserver.go) so both of this
// service's LAN surfaces make the same decision the same way.
func (s *controlServer) requirePin(h http.HandlerFunc) http.HandlerFunc {
	return requirePinnedPeer(s.mesh, h)
}

// mux builds the ec routes. Split from the listener so tests can exercise the
// handlers over httptest without binding a real port.
func (s *controlServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(controlEnginesPath, s.requirePin(s.handleEngines))
	mux.HandleFunc(controlInstallPath, s.requirePin(s.handleInstall))
	mux.HandleFunc(controlPullPath, s.requirePin(s.handlePull))
	mux.HandleFunc(controlLoadPath, s.requirePin(s.handleLoad))
	mux.HandleFunc(controlUnloadPath, s.requirePin(s.handleUnload))
	mux.HandleFunc(controlDeletePath, s.requirePin(s.handleDelete))
	mux.HandleFunc(controlStartPath, s.requirePin(s.handleStart))
	mux.HandleFunc(controlStopPath, s.requirePin(s.handleStop))
	return mux
}

// handleEngines serves this node's engine status list to a pinned peer — the
// same data engine:get-installed returns over stdio.
func (s *controlServer) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"engines": s.exec.GetInstalled()})
}

// serveControl runs the ec mTLS control surface on 0.0.0.0:port until ctx is
// cancelled. A bind failure is non-fatal: stdio engine management (and local
// peers' remote calls being unavailable) must not take the process down.
func serveControl(ctx context.Context, port int, exec *Executor, mesh *clustertrust.Mesh) {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("engine-manager control endpoint disabled (bind failed)", "addr", addr, "err", err)
		return
	}
	ln = tls.NewListener(ln, mesh.ServerTLSConfig())
	srv := &http.Server{
		Handler:           (&controlServer{exec: exec, mesh: mesh}).mux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	slog.Info("serving engine control surface (mTLS)",
		"addr", addr, "path", controlEnginesPath, "clustered", mesh.Clustered())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("engine-manager control endpoint exited", "err", err)
	}
}
