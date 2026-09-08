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
	"net"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
	"nvpair-shared/reach"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// ReadyParams is the payload of the one-shot "engine:ready" notification.
type ReadyParams struct {
	Version string `json:"version"`
}

type engineParam struct {
	Engine string `json:"engine"`
	Port   int    `json:"port,omitempty"`
}

// opParam is the lifecycle-op input. Port (start) and Start (install) are
// optional, per-call overrides; no manifest mutation, no persistence.
type opParam struct {
	Engine string `json:"engine"`
	Port   int    `json:"port,omitempty"`
	Bind   string `json:"bind,omitempty"`
	Start  bool   `json:"start,omitempty"`
	// Model is the one-shot served-model override for engine:start, mirroring
	// Port: it applies to this launch only and is never persisted. Use
	// engine:set-model for the persistent choice.
	Model string `json:"model,omitempty"`
}

type actionParam struct {
	Engine string          `json:"engine"`
	Action string          `json:"action"`
	Params json.RawMessage `json:"params,omitempty"`
}

// setPortParam is the engine:set-port input: the engine whose server port to
// change and the new port. The chosen port is persisted as a manifest
// override and applied (the engine is bounced onto it if running).
type setPortParam struct {
	Engine string `json:"engine"`
	Port   int    `json:"port"`
}

// setModelParam is the engine:set-model input: the engine whose served model to
// change and the model id. It is the persistent counterpart of the one-shot
// engine:start {model} override — the choice is written as a manifest override
// exactly the way engine:set-port writes the port, and an empty model removes
// the override (back to the bundled default).
type setModelParam struct {
	Engine string `json:"engine"`
	Model  string `json:"model"`
}

// Manager is the engine-manager's JSON-RPC front end. It dispatches the
// engine:* surface to the Executor. Long-running operations (install,
// start, stop, restart, action) run in their own goroutine so the read
// loop never blocks; the codec serializes the concurrent responses.
type Manager struct {
	codec *Codec
	exec  *Executor
	peers *peerDirectory
	addrs *reach.Chooser
	mesh  *clustertrust.Mesh // cluster identity + pins for dialing peers' ec surfaces
	// remoteHTTP / readyHTTP are long-lived per-peer mTLS pools. A throwaway
	// Transport per engine:remote-* call leaked the idle socket. readyHTTP
	// uses the longer header budget for start/delete (see waitsForEngineReadiness).
	remoteHTTP *clustertrust.PeerClientPool
	readyHTTP  *clustertrust.PeerClientPool
	cancel     context.CancelFunc
}

func NewManager(codec *Codec, exec *Executor, mesh *clustertrust.Mesh) *Manager {
	// cancel defaults to a no-op so the "shutdown" handler is safe even if
	// handleMessage is reached before Run() installs the real CancelFunc
	// (e.g. a unit test calling it directly); Run overwrites it.
	return &Manager{
		codec: codec,
		exec:  exec,
		peers: newPeerDirectory(),
		addrs: reach.NewChooser(),
		mesh:  mesh,
		remoteHTTP: clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{
			ResponseHeaderTimeout: remoteResponseHeaderTimeout,
		}),
		readyHTTP: clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{
			ResponseHeaderTimeout: remoteReadyResponseHeaderTimeout,
		}),
		cancel: func() {},
	}
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()

	if err := m.codec.Notify("engine:ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	// Subscribe to the broker's discovery relay for ec peers (nodes exposing the
	// remote-control surface). They arrive as discovery:nodes snapshots handled
	// in handleMessage. Non-fatal: the directory just stays empty if the parent
	// isn't a relay-aware broker, and remote calls then resolve to no target.
	if err := m.codec.Notify(noderec.MethodSubscribe, noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceEngineControl}}); err != nil {
		slog.Warn("failed to subscribe to discovery relay for ec peers", "err", err)
	}

	// Watch each running engine's loaded-model set and emit engine:models-changed
	// on change (load/unload, JIT auto-load, TTL eviction). Bound to ctx: stops
	// when the read loop exits below.
	go m.exec.watchLoaded(ctx)

	err := m.readLoop(ctx)
	// Cancel first so any in-flight start aborts at its readiness wait,
	// then stop engines so the parent's exit doesn't orphan them.
	cancel()
	m.exec.StopAll()
	m.remoteHTTP.CloseIdle()
	m.readyHTTP.CloseIdle()
	return err
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			var de *DecodeError
			if errors.As(err, &de) {
				log.Printf("JSON-RPC decode error (skipping frame): %v", err)
				continue
			}
			// Terminal transport/scanner error — stop instead of spinning.
			log.Printf("JSON-RPC read error (terminal): %v", err)
			return err
		}
		m.handleMessage(ctx, msg)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (m *Manager) handleMessage(ctx context.Context, msg *Message) {
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

	if msg.Method == noderec.NotifyNodes {
		m.applyDiscovery(msg)
		return
	}
	if msg.Method == restoreEnabledMethod && msg.IsNotification() {
		go func() {
			if err := m.exec.RestoreEnabled(ctx); err != nil {
				slog.Warn("one or more enabled engines could not be restored", "err", err)
			}
		}()
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
		if err := m.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()

	case "engine:get-installed":
		go func() {
			m.codec.Respond(msg.ID, map[string]any{"engines": m.exec.GetInstalled()})
		}()

	case prepareShutdownMethod:
		go func() {
			m.exec.StopAll()
			m.codec.Respond(msg.ID, nil)
		}()

	case "engine:describe":
		var p engineParam
		if !m.parse(msg, &p) {
			return
		}
		mf, ok := m.exec.reg.Get(p.Engine)
		if !ok {
			m.codec.RespondError(msg.ID, -32602, fmt.Sprintf("unknown engine %q", p.Engine))
			return
		}
		m.codec.Respond(msg.ID, mf)

	case "engine:status":
		var p engineParam
		if !m.parse(msg, &p) {
			return
		}
		go func() {
			st, err := m.exec.StatusAtPort(p.Engine, p.Port)
			m.respondOrErr(msg, st, err)
		}()

	case "engine:logs":
		var p engineParam
		if !m.parse(msg, &p) {
			return
		}
		lines, err := m.exec.Logs(p.Engine)
		m.respondOrErr(msg, map[string]any{"lines": lines}, err)

	case "engine:errors":
		m.codec.Respond(msg.ID, map[string]any{"errors": m.exec.Errors()})

	case "engine:models":
		go m.runModels(ctx, msg)

	case "engine:install", "engine:uninstall", "engine:start", "engine:stop", "engine:restart":
		go m.runOp(ctx, msg)

	case "engine:set-port":
		go m.runSetPort(ctx, msg)

	case "engine:set-model":
		go m.runSetModel(ctx, msg)

	case "internal:set-reserved-port":
		var p struct {
			Port int `json:"port"`
		}
		if !m.parse(msg, &p) {
			return
		}
		if err := m.exec.SetReservedPort(p.Port); err != nil {
			m.codec.RespondError(msg.ID, -32602, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]int{"port": p.Port})

	case "engine:action":
		go m.runAction(ctx, msg)

	case "engine:remote-get-installed", "engine:remote-install", "engine:remote-pull-model",
		"engine:remote-load-model", "engine:remote-unload-model", "engine:remote-delete-model",
		"engine:remote-start", "engine:remote-stop":
		go m.runRemote(ctx, msg)

	default:
		m.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method))
	}
}

// runOp executes a lifecycle op asynchronously and responds with the
// engine's resulting status (or an error).
func (m *Manager) runOp(ctx context.Context, msg *Message) {
	var p opParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Engine == "" {
		m.codec.RespondError(msg.ID, -32602, "engine is required")
		return
	}
	if p.Port < 0 || p.Port > 65535 {
		m.codec.RespondError(msg.ID, -32602, "port must be between 0 and 65535")
		return
	}
	if p.Bind != "" && net.ParseIP(p.Bind) == nil {
		m.codec.RespondError(msg.ID, -32602, "bind must be a valid IP address")
		return
	}
	start := func() error {
		return m.exec.StartWith(ctx, p.Engine, startOpts{Port: p.Port, Bind: p.Bind, Model: p.Model})
	}
	var err error
	switch msg.Method {
	case "engine:install":
		if err = m.exec.Install(ctx, p.Engine); err == nil && p.Start {
			err = start()
		}
	case "engine:uninstall":
		err = m.exec.Uninstall(ctx, p.Engine)
	case "engine:start":
		err = start()
	case "engine:stop":
		err = m.exec.Stop(p.Engine)
	case "engine:restart":
		err = m.exec.Restart(ctx, p.Engine)
	}
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	st, err := m.exec.Status(p.Engine)
	m.respondOrErr(msg, st, err)
}

// runSetPort persists an engine's server port (as a manifest override) and
// applies it, responding with the engine's resulting status.
func (m *Manager) runSetPort(ctx context.Context, msg *Message) {
	var p setPortParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Engine == "" {
		m.codec.RespondError(msg.ID, -32602, "engine is required")
		return
	}
	if p.Port < 1 || p.Port > 65535 {
		m.codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535")
		return
	}
	st, err := m.exec.SetPort(ctx, p.Engine, p.Port)
	m.respondOrErr(msg, st, err)
}

// runSetModel persists the model an engine serves (as a manifest override) and
// applies it, responding with the engine's resulting status.
func (m *Manager) runSetModel(ctx context.Context, msg *Message) {
	var p setModelParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Engine == "" {
		m.codec.RespondError(msg.ID, -32602, "engine is required")
		return
	}
	st, err := m.exec.SetModel(ctx, p.Engine, p.Model)
	m.respondOrErr(msg, st, err)
}

// runModels answers engine:models with the union of running engines' model
// lists plus per-engine attribution (modelsByEngine). It runs async (like
// engine:action) because it makes one loopback HTTP call per running engine and
// must not block the read loop.
func (m *Manager) runModels(ctx context.Context, msg *Message) {
	m.codec.Respond(msg.ID, m.exec.ModelsResult(ctx))
}

func (m *Manager) runAction(ctx context.Context, msg *Message) {
	var p actionParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Engine == "" || p.Action == "" {
		m.codec.RespondError(msg.ID, -32602, "engine and action are required")
		return
	}
	// A model pull is routed through the streaming path so a local pull emits
	// live engine:pull-progress just like a remote pull relays
	// engine:remote-progress; the response is still the action's terminal result
	// rather than a buffered progress blob.
	var res json.RawMessage
	var err error
	if p.Action == pullModelAction {
		model := modelFromParams(p.Params)
		res, err = m.exec.PullModelStream(ctx, p.Engine, model, p.Params)
		if err != nil {
			// A pull can fail after the client's synchronous call has already
			// timed out (long downloads), so the RPC error alone can't reach a
			// UI that stopped waiting. Emit one terminal engine:pull-progress
			// error frame so a local subscriber converges off "pulling" even in
			// that case, mirroring install's failed progress step.
			userMsg := m.exec.reportPullFailed(p.Engine, model, err)
			m.exec.emitPullProgress(ProgressEvent{Engine: p.Engine, Op: "pull", Stage: "error", Percent: -1, Message: userMsg})
			m.codec.RespondError(msg.ID, -32000, userMsg)
			return
		}
	} else {
		res, err = m.exec.Action(ctx, p.Engine, p.Action, p.Params)
	}
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	// An action may have changed what's resident (load/unload/pull/delete/run, or
	// a chat that JIT-loads); nudge the loaded watcher so engine:models-changed
	// fires promptly instead of waiting for the next poll tick. Pure reads
	// (list_models/loaded_models/list_downloaded) can't move residency, so skip
	// the nudge and its wasted sweep for them.
	if !residencyNeutralActions[p.Action] {
		m.exec.pokeLoaded()
	}
	m.codec.Respond(msg.ID, res)
}

// applyDiscovery folds a discovery:nodes snapshot (the broker relay's full
// filtered ec set) into the peer directory, replacing it wholesale.
func (m *Manager) applyDiscovery(msg *Message) {
	var res noderec.GetNodesResult
	if err := json.Unmarshal(msg.Params, &res); err != nil {
		slog.Debug("invalid discovery:nodes snapshot", "err", err)
		return
	}
	m.peers.set(res.Nodes)
}

func (m *Manager) parse(msg *Message, v any) bool {
	if err := json.Unmarshal(msg.Params, v); err != nil {
		m.codec.RespondError(msg.ID, -32602, "invalid params: "+err.Error())
		return false
	}
	return true
}

func (m *Manager) respondOrErr(msg *Message, result any, err error) {
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	m.codec.Respond(msg.ID, result)
}
