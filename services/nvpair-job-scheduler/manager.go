// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/schedulerwire"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// intervalFloor is the smallest recompute cadence accepted; a shorter request
// is clamped up to this (spec §7.5).
const intervalFloor = 200 * time.Millisecond

// ReadyParams is the payload of the startup "ready" notification.
type ReadyParams struct {
	Version string `json:"version"`
}

// Manager owns the scheduler's in-memory state and the JSON-RPC read loop. It
// speaks only to its parent (the broker) over the codec: it consumes fanned
// discovery/workload notifications, and emits schedule:priority notifications.
// It never originates a request upward.
type Manager struct {
	codec  *Codec
	cancel context.CancelFunc

	// mu guards all in-memory state below: the recompute cadence, the node
	// universe (set of discovered ids), the workload catalog, GPU telemetry,
	// and the last-emitted order per engine.
	mu        sync.Mutex
	interval  time.Duration
	nodes     map[string]bool
	catalog   map[wlKey]workload
	telemetry map[string]gpuTelemetryState
	emitted   map[string]engineState

	// recomputeMu serializes event, timer, and forced recomputes through their
	// notifications. An older timer result therefore cannot be emitted after a
	// newer workload result.
	recomputeMu sync.Mutex

	// tickCh signals a forced recompute (scheduler:tick). Buffered so a tick
	// request never blocks on the loop.
	tickCh chan struct{}
}

// NewManager builds a scheduler manager with the given recompute cadence
// (clamped to the floor).
func NewManager(codec *Codec, interval time.Duration) *Manager {
	if interval < intervalFloor {
		interval = intervalFloor
	}
	return &Manager{
		codec:     codec,
		interval:  interval,
		nodes:     make(map[string]bool),
		catalog:   make(map[wlKey]workload),
		telemetry: make(map[string]gpuTelemetryState),
		emitted:   make(map[string]engineState),
		tickCh:    make(chan struct{}, 1),
	}
}

// Run emits the ready notification and drives the read loop until the parent
// closes the pipe (stdin EOF) or a shutdown is requested.
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	go m.scheduleLoop(ctx)

	return m.readLoop(ctx)
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
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
		m.handleMessage(msg)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (m *Manager) handleMessage(msg *Message) {
	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				m.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			m.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			changed := false
			switch msg.Method {
			case "discovery:nodes-changed":
				changed = m.applyNodesChanged(msg.Params)
			case "workloads:upsert":
				changed = m.applyUpsert(msg.Params)
			case "workloads:remove":
				changed = m.applyRemove(msg.Params)
			case schedulerwire.MethodTelemetry:
				changed = m.applyTelemetry(msg.Params)
			default:
				slog.Debug("ignoring notification", "method", msg.Method)
			}
			if changed {
				m.recomputeAll(false)
			}
		}
		return
	}

	switch msg.Method {
	case "scheduler:get-status":
		if err := m.codec.Respond(msg.ID, m.status()); err != nil {
			log.Printf("failed to respond to scheduler:get-status: %v", err)
		}

	case "scheduler:get-interval":
		m.mu.Lock()
		d := m.interval
		m.mu.Unlock()
		if err := m.codec.Respond(msg.ID, map[string]int64{"interval_ms": d.Milliseconds()}); err != nil {
			log.Printf("failed to respond to scheduler:get-interval: %v", err)
		}

	case "scheduler:set-interval":
		var p struct {
			IntervalMs int64 `json:"interval_ms"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.IntervalMs <= 0 {
			m.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"interval_ms\": <positive int>}")
			return
		}
		d := time.Duration(p.IntervalMs) * time.Millisecond
		if d < intervalFloor {
			d = intervalFloor
		}
		m.mu.Lock()
		m.interval = d
		m.mu.Unlock()
		slog.Info("interval changed", "interval_ms", d.Milliseconds())
		if err := m.codec.Respond(msg.ID, map[string]int64{"interval_ms": d.Milliseconds()}); err != nil {
			log.Printf("failed to respond to scheduler:set-interval: %v", err)
		}

	case "scheduler:tick":
		select {
		case m.tickCh <- struct{}{}:
		default:
		}
		if err := m.codec.Respond(msg.ID, map[string]bool{"ticked": true}); err != nil {
			log.Printf("failed to respond to scheduler:tick: %v", err)
		}

	case "shutdown":
		if err := m.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()
	default:
		if err := m.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}
