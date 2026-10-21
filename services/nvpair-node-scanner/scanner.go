// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"

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
