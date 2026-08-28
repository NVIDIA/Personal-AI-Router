// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

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
	"nvpair-shared/splitlisten"
)

// modelsPath is the LAN endpoint a peer's discovery daemon fetches to enrich a
// node's model list. Versioned so future remote engine operations can live
// alongside it under /v1.
const modelsPath = "/v1/models"

// serveHTTP runs engine-manager's model-list surface (the em service) on
// 0.0.0.0:port until ctx is cancelled, backed by exec.Models. The model list
// moved off the mDNS TXT record (which couldn't hold an unbounded list) onto
// HTTP, and engine-manager owns it because it already owns engine state. A bind
// failure is non-fatal: engine management over stdio must keep working even if
// the LAN endpoint can't come up.
//
// A node's model inventory is cluster data, so the LAN side is cluster mTLS: it
// tells anyone who asks which models this machine holds, which is inventory of
// the same kind the workload and error surfaces protect. Treating model names as
// non-sensitive because they used to ride in an mDNS TXT record was a blind spot
// — the TXT broadcast was itself the thing being replaced.
//
// One port carries both personalities, split by each connection's first byte:
//
//   - plain HTTP, restricted to loopback. This is how THIS node's own scanner
//     enriches its own card (it always dials 127.0.0.1 for self, both on publish
//     and on the periodic model refresh), and it must keep working when this node
//     belongs to no cluster — a standalone machine still has to show its own
//     models. A non-loopback plaintext caller is refused.
//   - cluster mTLS, restricted to pinned peers. The leaf is resolved per
//     handshake from live membership, so an unclustered node presents none and no
//     LAN caller gets in at all, and a node that joins starts serving its peers on
//     the same listener with no rebind.
func serveHTTP(ctx context.Context, port int, exec *Executor, mesh *clustertrust.Mesh) {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	models := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// {"models":[...union...],"modelsByEngine":{"ollama":[...],...},
		// "loadedByEngine":{"ollama":[...],...}} — the flat union stays for
		// existing consumers; modelsByEngine and loadedByEngine are additive
		// (both omitempty, so absent when no engine reports them).
		_ = json.NewEncoder(w).Encode(exec.ModelsResult(r.Context()))
	})

	plainMux := http.NewServeMux()
	plainMux.Handle(modelsPath, loopbackOnly(models))
	tlsMux := http.NewServeMux()
	tlsMux.Handle(modelsPath, requirePinnedPeer(mesh, models))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("engine-manager HTTP endpoint disabled (bind failed)", "addr", addr, "err", err)
		return
	}
	split := splitlisten.New(ln)
	plainSrv := &http.Server{Handler: plainMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: clustertrust.PeerListenerIdleTimeout}
	tlsSrv := &http.Server{Handler: tlsMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: clustertrust.PeerListenerIdleTimeout}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = plainSrv.Shutdown(shutCtx)
		_ = tlsSrv.Shutdown(shutCtx)
		_ = split.Close()
	}()
	slog.Info("serving engine model surface", "addr", addr, "path", modelsPath,
		"clustered", mesh.Clustered())
	go serveOrWarn(tlsSrv, tls.NewListener(split.TLS(), mesh.ServerTLSConfig()))
	serveOrWarn(plainSrv, split.Plain())
}

// serveOrWarn runs srv on l, treating a normal shutdown (and the sub-listener
// closing under it) as success.
func serveOrWarn(srv *http.Server, l net.Listener) {
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		slog.Warn("engine-manager HTTP endpoint exited", "err", err)
	}
}

// loopbackOnly restricts h to callers on this machine. A LAN caller must use the
// mTLS personality instead, so plaintext access can never widen past the host
// that owns the engines.
func loopbackOnly(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			slog.Warn("rejected non-loopback plaintext model request; cluster peers must use mTLS",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "forbidden: plaintext model requests are accepted only from loopback", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}
}

// requirePinnedPeer gates h on cluster-peer mTLS: the authenticated caller must
// present a certificate this node currently pins. Membership and pins are
// re-derived first, so a cluster joined or left, or a peer paired or removed,
// after startup is reflected without a restart.
//
// Unconditional by design — not "if this node is clustered". VerifyClientPin is
// already false for a node that belongs to no cluster, so wrapping it in a
// membership test would be the one thing that could reopen the surface.
func requirePinnedPeer(mesh *clustertrust.Mesh, h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mesh.Refresh()
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}
}

// isLoopbackRemote reports whether an http.Request RemoteAddr is a loopback
// address.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
