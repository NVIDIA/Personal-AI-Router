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
	"sync"

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

// Manager is the engine-manager's JSON-RPC front end. It dispatches the
// engine:* surface to the Executor. Long-running operations (install,
// start, stop, restart, action) run in their own goroutine so the read
// loop never blocks; the codec serializes the concurrent responses.
type Manager struct {
	onboarding    *onboardingService
	cables        *cableProductService
	settingsRelay settingsRelay
	codec         *Codec
	exec          *Executor
	peers         *peerDirectory
	addrs         *reach.Chooser
	mesh          *clustertrust.Mesh // cluster identity + pins for dialing peers' ec surfaces
	cableLocal    *cableLocalFacts
	// remoteHTTP / readyHTTP are long-lived per-peer mTLS pools. A throwaway
	// Transport per engine:remote-* call leaked the idle socket. readyHTTP
	// uses the longer header budget for start/delete (see waitsForEngineReadiness).
	remoteHTTP   *clustertrust.PeerClientPool
	readyHTTP    *clustertrust.PeerClientPool
	groupRoute   *vllmGroupRouteGate
	cancel       context.CancelFunc
	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func NewManager(codec *Codec, exec *Executor, mesh *clustertrust.Mesh) *Manager {
	m := newManagerTransport(codec, exec, mesh)
	m.settingsRelay.send = codec.Notify
	exec.settingsParent = m.settingsRelay.call
	m.groupRoute = newVLLMGroupRouteGate()
	exec.groupPeer = &vllmGroupPeer{m: m}
	exec.groupPeer.bootstrapSystemOwner()
	m.onboarding = newOnboardingService(m)
	exec.diagnostics = newDiagnosticService(m)
	m.cables = newCableProductService(m)
	exec.fabric = newFabricService(m)
	return m
}

func newManagerTransport(codec *Codec, exec *Executor, mesh *clustertrust.Mesh) *Manager {
	// cancel defaults to a no-op so the "shutdown" handler is safe even if
	// handleMessage is reached before Run() installs the real CancelFunc
	// (e.g. a unit test calling it directly); Run overwrites it.
	m := &Manager{
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
	return m
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()
	m.onboarding.ctx = ctx
	m.cables.ctx = ctx
	m.exec.fabric.ctx = ctx
	m.exec.diagnostics.ctx = ctx
	m.exec.diagnostics.recover(ctx)

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
	go m.watchPeerSettings(ctx)

	err := m.readLoop(ctx)
	// Cancel first so any in-flight start aborts at its readiness wait,
	// then stop engines so the parent's exit doesn't orphan them.
	cancel()
	err = errors.Join(err, m.shutdown())
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
	if msg.Method == "engine:diagnostic-workload-state" {
		m.exec.diagnostics.receiveWorkloads(msg.Params)
		return
	}
	if msg.Method == "engine:vllm-group-route-state" {
		if m.groupRoute != nil {
			m.groupRoute.receive(msg.Params)
		}
		return
	}
	if msg.Method == "engine:onboarding-cluster-response" {
		m.onboarding.clusterReply(msg.Params)
		return
	}
	switch msg.Method {
	case "engine:diagnostic-mpi-review", "engine:diagnostic-mpi-approve", "engine:diagnostic-mpi-status", "engine:diagnostic-mpi-cancel", "engine:diagnostic-mpi-recover", "engine:diagnostic-mpi-reconcile":
		if msg.IsRequest() {
			go m.handleDiagnosticMPI(ctx, msg)
		}
		return
	case "engine:diagnostic-runtime-review", "engine:diagnostic-runtime-approve", "engine:diagnostic-runtime-status", "engine:diagnostic-runtime-cancel", "engine:diagnostic-runtime-retry", "engine:diagnostic-runtime-adopt", "engine:diagnostic-managed-runtime", "engine:diagnostic-managed-runtimes":
		if msg.IsRequest() {
			go m.handleDiagnosticRuntime(ctx, msg)
		}
		return
	case "engine:vllm-python-prepare-review", "engine:diagnostic-package-review", "engine:diagnostic-package-approve", "engine:diagnostic-package-status", "engine:diagnostic-package-cancel", "engine:diagnostic-package-retry":
		if msg.IsRequest() {
			go m.handleDiagnosticPackages(ctx, msg)
		}
		return
	case "engine:diagnostic-targets", "engine:diagnostic-inspect":
		if msg.IsRequest() {
			go m.handleDiagnosticInspection(ctx, msg)
		}
		return
	case "engine:diagnostic-setup-review":
		if msg.IsRequest() {
			go m.handleDiagnosticSetupReview(msg)
		}
		return
	case "engine:diagnostic-groups", "engine:diagnostic-start", "engine:diagnostic-status", "engine:diagnostic-cancel":
		if msg.IsRequest() {
			go m.handleDiagnostic(ctx, msg)
		}
		return
	case "engine:onboarding-candidates", "engine:onboarding-add-target", "engine:onboarding-access", "engine:onboarding-inspect", "engine:onboarding-approve", "engine:onboarding-status", "engine:onboarding-cancel", "engine:onboarding-retry", "engine:onboarding-scopes", "engine:onboarding-discover", "engine:onboarding-import-artifact", "engine:onboarding-bootstrap-catalog", "engine:onboarding-bootstrap-controller-keys", "engine:onboarding-bootstrap-inspect", "engine:onboarding-bootstrap-review", "engine:onboarding-bootstrap-apply", "engine:onboarding-bootstrap-status", "engine:onboarding-bootstrap-recover", "engine:onboarding-bootstrap-verify":
		if msg.IsRequest() {
			go m.handleOnboarding(ctx, msg)
		}
		return
	}
	if m.handleSettingsMessage(ctx, msg) {
		return
	}
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
		m.exec.diagnostics.closePackageAdmission()
		if err := m.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()

	case "engine:get-installed":
		go func() {
			m.codec.Respond(msg.ID, map[string]any{"engines": m.exec.GetInstalledContext(ctx)})
		}()

	case prepareShutdownMethod:
		m.exec.diagnostics.closePackageAdmission()
		go func() {
			m.respondOrErr(msg, nil, m.shutdown())
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

	case "engine:onboarding-history":
		// Read-only compatibility inventory. It cannot create or resume an
		// onboarding operation and therefore carries no runtime authority.
		history := m.exec.OnboardingHistory()
		history.DiagnosticRecoveryRequired = m.exec.diagnostics.recoveryRequired()
		m.codec.Respond(msg.ID, history)

	case "engine:vllm-group-review", "engine:vllm-group-check", "engine:vllm-group-start",
		"engine:vllm-group-stop", "engine:vllm-group-reconcile":
		go m.handleVLLMGroup(ctx, msg)

	case "engine:vllm-group-status":
		m.handleVLLMGroup(ctx, msg)

	case "engine:vllm-group-cleanup":
		// Binding-only in this generation. The backend re-reads the retained
		// journal and returns an explicit admission hold without effects.
		var p vllmGroupCleanupRequest
		if !m.parse(msg, &p) {
			return
		}
		go func() {
			status, err := m.exec.ReconcileVLLMGroupCleanup(ctx, p)
			m.respondOrErr(msg, status, err)
		}()

	case "engine:vllm-select-model":
		var p vllmSelectModelRequest
		if !m.parse(msg, &p) {
			return
		}
		go func() {
			result, err := m.exec.SelectVLLMModel(ctx, p.Model)
			m.respondOrErr(msg, result, err)
		}()

	case "engine:vllm-qwen38-prepare":
		go m.runQwen38Prepare(ctx, msg)

	case "engine:vllm-distribute-model":
		var request vllmDistributionRequest
		if !m.parse(msg, &request) {
			return
		}
		if request.OpID == "" {
			request.OpID = request.OperationID
		}
		go func() {
			result, err := m.distributeVLLMModelHere(ctx, request)
			m.respondOrErr(msg, result, err)
		}()

	case "engine:models":
		go m.runModels(ctx, msg)

	case "engine:install", "engine:update", "engine:uninstall", "engine:start", "engine:stop", "engine:restart":
		go m.runOp(ctx, msg)

	case "engine:set-port":
		go m.runSetPort(ctx, msg)

	case "engine:cable-review":
		go m.runCableReview(ctx, msg)
	case "engine:fabric-inventory":
		var p struct {
			NodeIDs []string `json:"nodeIds"`
		}
		if !m.parse(msg, &p) {
			return
		}
		go func() {
			if m.exec.fabric == nil {
				m.codec.RespondError(msg.ID, -32000, "fabric inventory is unavailable")
				return
			}
			inventory, err := m.exec.fabric.topologyInventory(ctx, p.NodeIDs)
			m.respondOrErr(msg, inventory, err)
		}()
	case "engine:fabric-review", "engine:fabric-approve", "engine:fabric-status", "engine:fabric-cancel", "engine:fabric-recover", "engine:fabric-retained-operations":
		go m.handleFabric(ctx, msg)
	case "engine:cable-start", "engine:cable-status", "engine:cable-cancel":
		go m.runCableProduct(ctx, msg)
	case "engine:cable-retained-runs":
		go m.runCableRetained(ctx, msg)
	case "engine:cable-cleanup-review", "engine:cable-cleanup-verify", "engine:cable-cleanup-cancel":
		go m.runCableCleanup(ctx, msg)

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

	case "engine:remote-get-installed", "engine:remote-install", "engine:remote-pull-model", "engine:remote-vllm-qwen38-prepare", "engine:remote-distribute-model",
		"engine:remote-load-model", "engine:remote-unload-model", "engine:remote-delete-model",
		"engine:remote-update", "engine:remote-start", "engine:remote-stop", "engine:remote-cancel-pull":
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
		return m.exec.StartWith(ctx, p.Engine, startOpts{Port: p.Port, Bind: p.Bind})
	}
	var err error
	switch msg.Method {
	case "engine:install":
		if err = m.exec.Install(ctx, p.Engine); err == nil && p.Start {
			err = start()
		}
	case "engine:update":
		err = m.exec.Update(ctx, p.Engine)
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

// runModels answers engine:models with the union of running engines' served
// model lists, per-engine attribution, and separate retained catalogs. It runs async (like
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
			if errors.Is(err, context.Canceled) {
				m.exec.emitPullProgress(ProgressEvent{Engine: p.Engine, Op: "pull", Stage: "cancelled", Percent: -1, Message: model})
				m.codec.RespondError(msg.ID, -32000, "model download cancelled")
				return
			}
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
	if m.mesh != nil {
		m.mesh.Refresh()
		self := m.mesh.NodeUUID()
		for _, node := range res.Nodes {
			if self != "" && node.ClusterUUID == self {
				if m.cableLocal == nil {
					m.cableLocal = newCableLocalFacts(node.HostUUID, 14318)
				} else {
					m.cableLocal.nodeID = node.HostUUID
				}
				m.exec.vllmNodeID = node.HostUUID
				break
			}
		}
	}
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
