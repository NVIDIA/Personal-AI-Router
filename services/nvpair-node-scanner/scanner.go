// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// GPUInfo / CPUInfo / MemoryInfo are the node-info enrichment shapes, single-
// sourced in nvpair-shared/noderec so the daemon can decorate a DirectoryNode
// directly.
type (
	GPUInfo    = noderec.GPUInfo
	CPUInfo    = noderec.CPUInfo
	MemoryInfo = noderec.MemoryInfo
)

// NodeInfoResponse is the /v1/node-info body the daemon fetches to enrich a
// node. Share the hardware contract with the local IPC path to prevent drift.
// A nil ClusterUUID is unknown membership, while a pointer to "" is unclustered.
type NodeInfoResponse = noderec.NodeInfo

type ReadyParams struct {
	Version string `json:"version"`
}

// Scanner owns the discovery daemon's process lifecycle and routes stdio
// JSON-RPC frames into its discovery:* handlers.
type Scanner struct {
	codec  *Codec
	daemon *daemon
	cancel context.CancelFunc
}

// NewScanner uses the live mesh for authenticated discovery. clusterDir also
// selects the local identity root shared with cluster-manager.
func NewScanner(codec *Codec, mesh *clustertrust.Mesh, clusterDir string) *Scanner {
	return &Scanner{
		codec:  codec,
		daemon: newDaemon(codec, mesh, clusterDir),
	}
}

func (s *Scanner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	if err := s.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		s.daemon.run(ctx)
	}()

	err := s.readLoop(ctx)
	// The daemon's mDNS responder sends its RFC 6762 §10.1 shutdown
	// retraction only after ctx is cancelled, and readLoop's stdin-EOF
	// return path is exactly such a moment with ctx still live (the broker
	// vanished; nothing cancelled us). Cancel explicitly — the kept
	// defer cancel() would only run after this wait, and the daemon's join
	// would deadlock on a context nobody cancels — then wait for the daemon
	// so the retraction has left the machine before Run returns and main
	// may exit.
	cancel()
	waitForShutdown(daemonDone, shutdownJoinTimeout, "discovery daemon")
	return err
}

// shutdownJoinTimeout bounds the waits Scanner.Run and daemon.run take for
// the mDNS responder's shutdown retraction to finish. A send is one small
// multicast per interface, so the timeout only fires when a socket send
// itself stalls; dropping the retraction is then the price of a bounded
// shutdown, which beats hanging the broker's wait forever.
const shutdownJoinTimeout = 2 * time.Second

// waitForShutdown waits for done to close, bounded by timeout. A nil done
// means the responder is disabled and there is nothing to wait for. The
// timeout logs at Warn because the cost of giving up is every peer holding
// our dead node's records until the record TTL expires.
func waitForShutdown(done <-chan struct{}, timeout time.Duration, what string) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("shutdown wait gave up before component returned",
			"component", what, "timeout", timeout)
	}
}

func (s *Scanner) readLoop(ctx context.Context) error {
	for {
		msg, err := s.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			continue
		}
		s.handleMessage(msg)
	}
}

func (s *Scanner) handleMessage(msg *Message) {
	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				s.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			s.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}

	// discovery:* methods (register/unregister/update-txt/get-nodes),
	// which may arrive as requests (acked) or notifications, before the
	// request-only guard below.
	if s.daemon.handle(msg) {
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "shutdown":
		if err := s.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		s.cancel()

	default:
		if err := s.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}
