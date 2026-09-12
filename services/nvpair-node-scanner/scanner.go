// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"

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
// node (see daemon.fetchNodeInfo).
type NodeInfoResponse struct {
	GPUs           []GPUInfo   `json:"GPUs"`
	CPU            *CPUInfo    `json:"cpu,omitempty"`
	Memory         *MemoryInfo `json:"memory,omitempty"`
	TelemetryValid bool        `json:"telemetryValid"`
	MSSince        int64       `json:"msSince"`
	// HostUUID is the identity the node itself reports (nvpair-node-info's
	// hostUuid). It is not enrichment data — it is what lets the liveness probe
	// tell "this node still answers" from "some other node now answers at this
	// address", which is the difference between a live peer and the ghost a
	// wiped-and-reinstalled node leaves behind. Empty when the peer predates the
	// field, in which case the probe falls back to its TCP-only behaviour.
	HostUUID string `json:"hostUuid,omitempty"`
	// ClusterUUID is the node's own report of the cluster principal it holds,
	// empty when it belongs to no cluster. A pointer so absent is distinguishable
	// from empty: a node too old to report the field says nothing about its
	// membership, while a node reporting "" is asserting it has none. Reading
	// absent as unclustered would mark a clustered peer invitable.
	ClusterUUID *string `json:"clusterUuid"`
}

type ReadyParams struct {
	Version string `json:"version"`
}

// Scanner is the stdio/JSON-RPC front end for the promoted discovery daemon.
// Post-cutover there is no legacy _nvpair-node-info browse or node/* stream: the
// daemon advertises one _nvpair-node record, browses _nvpair-node, maintains the
// directory, and serves the discovery:* RPC. This wrapper owns the process
// lifecycle and routes stdio frames into the daemon.
type Scanner struct {
	codec  *Codec
	daemon *daemon
	cancel context.CancelFunc
}

// NewScanner builds the scanner around the promoted daemon. mesh (non-nil when
// this node is clustered) is threaded to the daemon for the trust annotation on
// browsed nodes; clusterDir lets the daemon resolve its node identity from the
// same base cluster-manager persists to (empty selects the shared default).
// tlsHTTP is nil unless the operator opted node-info enrichment onto HTTPS via
// the --client-cert/--client-key/--ca-bundle flags.
func NewScanner(codec *Codec, mesh *clustertrust.Mesh, clusterDir string, tlsHTTP *http.Client) *Scanner {
	return &Scanner{
		codec:  codec,
		daemon: newDaemon(codec, mesh, clusterDir, tlsHTTP),
	}
}

func (s *Scanner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()

	if err := s.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	go s.daemon.run(ctx)

	return s.readLoop(ctx)
}

func (s *Scanner) readLoop(ctx context.Context) error {
	for {
		msg, err := s.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			var de *DecodeError
			if errors.As(err, &de) {
				log.Printf("JSON-RPC decode error (skipping frame): %v", err)
				continue
			}
			// Terminal transport/scanner error (e.g. an over-long frame —
			// bufio.Scanner cannot resync) — stop instead of spinning.
			log.Printf("JSON-RPC read error (terminal): %v", err)
			return err
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
