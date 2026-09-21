// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// engineproxy.go is the broker's per-engine proxy table. Cross-process
// identity — name, namespace, facade and backend ports — comes from
// nvpair-shared/engines so the proxy, scheduler and TUI cannot disagree with
// the broker about it. What this file adds is the one thing only the broker
// needs: whether it may reposition the engine's process while it is running.
//
// That single question decides every place the two engines' port choreography
// diverges, which is why it is a named enum rather than a set of booleans or a
// bag of function pointers. A hook would only move the divergent bodies into
// this file; naming the reason keeps them where they belong and makes the
// difference reviewable.

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync/atomic"

	"nvpair-shared/engines"
	"nvpair-shared/errors"
	"nvpair-shared/noderec"
)

// engineOwnership answers a single question: may the broker reposition this
// engine's process while it is running?
type engineOwnership int

const (
	// adoptedEngine — the engine may have been started outside PAIR, so the
	// broker has no takeover authority over it. It is repositioned only while
	// stopped, and even then the move is deferred until the proxy has proved
	// it holds the facade port. Ollama.
	adoptedEngine engineOwnership = iota

	// managedEngine — engine-manager launched it in identified command mode
	// and has an official stop command for it, so it may be stopped and
	// repositioned before the proxy starts. LM Studio.
	managedEngine
)

// engineProxyProfile is everything the broker needs to supervise one engine's
// proxy.
type engineProxyProfile struct {
	engines.Engine

	// Ownership decides the port choreography: plan-then-commit for an
	// adopted engine, move-then-verify for a managed one. It is the only
	// judgment call in adding an engine.
	Ownership engineOwnership

	// HealthProbePath is the path whose 200 means "this engine is answering".
	// Ollama's liveness convention is a plain GET of the root; an
	// OpenAI-compatible engine answers on its model list.
	HealthProbePath string
}

// mayMoveRunningEngine reports whether engine-manager can be asked to
// reposition this engine while it is running. When it cannot, a facade port
// that is already occupied is a hard block: the broker has no way to tell its
// own engine from a stranger, and no authority to stop either.
func (p engineProxyProfile) mayMoveRunningEngine() bool {
	return p.Ownership == managedEngine
}

// blocksOnOccupiedFacade reports whether planning must refuse up front when the
// facade port is unavailable.
//
// Only an adopted engine can answer this before the move. For a managed engine
// the occupant is usually the engine itself, and availability cannot
// distinguish that from a stranger — so it moves the backend first and
// re-checks the facade afterwards, when a still-busy port genuinely means
// someone else. Applying the adopted rule to a managed engine would block the
// most common install, where LM Studio is already running on its facade port.
func (p engineProxyProfile) blocksOnOccupiedFacade() bool {
	return p.Ownership == adoptedEngine
}

// engineProxyRuntime is one engine's mutable proxy state. The broker holds one
// per engine, so adding an engine adds a table entry rather than another block
// of parallel fields on Broker.
//
// Locking is inherited from the Broker fields each of these replaced and is
// noted per field. The map itself is built once and never mutated afterwards,
// so resolving a runtime needs no lock — only its fields do.
type engineProxyRuntime struct {
	// profile is the descriptor this runtime belongs to.
	profile engineProxyProfile

	// proxy is the live child process, or nil. Guarded by Broker.workersMu: a
	// supervisor swaps it on restart from the monitor goroutine while request
	// handlers read it from the read loop.
	proxy *proxyProcess

	// subscribed reports whether a client asked for this engine's relay
	// stream. Guarded by Broker.proxyMu.
	subscribed bool

	// managedFacade reports that the broker is holding this engine's facade
	// port for its proxy.
	managedFacade atomic.Bool
	// explicitSettings keeps automatic bind recovery from overriding a saved
	// user choice. The reader must inspect this without taking engineConfigMu,
	// which a settings operation may hold while waiting for the same reader.
	explicitSettings atomic.Bool

	// backendPort is engine-manager's configured port for the engine itself,
	// as last observed. Zero means not yet known, which several paths treat
	// as a reason to fail closed rather than assume a default.
	backendPort atomic.Int32

	// startupPort is the explicit port the next spawn should bind. Zero lets
	// the proxy restore its own persisted port.
	startupPort atomic.Int32
}

// Deliberately not here yet: the ownership gate channel and its sync.Once, the
// respawn generations, and the reconcile mutex.
//
// The gate channel's nil-ness is load-bearing — a nil channel means "no gate
// configured", which is how a test that constructs a bare &Broker{} gets
// ungated relays. Moving it behind a lazily-built runtime would quietly make
// every such broker gated instead. The generations are worse: Ollama's lives
// under ollamaHostAliasMu because it is bumped together with the alias
// snapshot, while LM Studio's is a plain atomic, and collapsing that
// difference is what deadlocks the JSON-RPC read pump.

// engineProxy resolves an engine's runtime state.
//
// The map is initialized on first use rather than in NewBroker because a large
// body of tests constructs a bare &Broker{} and drives one behavior directly;
// requiring a constructor would mean touching all of them to change nothing.
// A zero sync.Once works on such a value, and Do gives every later reader a
// happens-before edge on the map.
func (b *Broker) engineProxy(p engineProxyProfile) *engineProxyRuntime {
	b.engineProxiesOnce.Do(func() {
		b.engineProxies = make(map[string]*engineProxyRuntime, len(engineProxyProfiles))
		for _, profile := range engineProxyProfiles {
			b.engineProxies[profile.Name] = &engineProxyRuntime{profile: profile}
		}
	})
	return b.engineProxies[p.Name]
}

// ollamaState and lmstudioState are shorthand for the two engines this build
// ships, for code that is inherently about one of them. Profile-generic code
// should take an engineProxyProfile and call engineProxy instead.
func (b *Broker) ollamaState() *engineProxyRuntime   { return b.engineProxy(ollamaProxyProfile) }
func (b *Broker) lmstudioState() *engineProxyRuntime { return b.engineProxy(lmstudioProxyProfile) }

// engineProxyProfiles is the broker's engine set, in preparation order.
// Ollama is first because its preparation reserves any inherited OLLAMA_HOST
// alias that later engines must route around.
var engineProxyProfiles = buildEngineProxyProfiles()

func buildEngineProxyProfiles() []engineProxyProfile {
	brokerOnly := map[string]engineProxyProfile{
		"ollama": {Ownership: adoptedEngine, HealthProbePath: "/"},
		// LM Studio is the one engine engine-manager may move while running:
		// its identified command-mode runtime has an official stop command.
		"lmstudio": {Ownership: managedEngine, HealthProbePath: "/v1/models"},
	}
	out := make([]engineProxyProfile, 0, len(engines.All()))
	for _, e := range engines.All() {
		p := brokerOnly[e.Name]
		p.Engine = e
		out = append(out, p)
	}
	return out
}

// enableFacadeRequest is the broker's facade/enable payload. It mirrors the
// child's parameter struct; the port and alias addresses travel here rather
// than on argv because the broker plans a different port for each engine.
type enableFacadeRequest struct {
	Engine              string   `json:"engine"`
	Port                int      `json:"port,omitempty"`
	AliasAddresses      []string `json:"aliasAddresses,omitempty"`
	IgnorePersistedPort bool     `json:"ignorePersistedPort,omitempty"`
}

// enableFacadeReply is where the facade actually landed. The bound port is read
// from here rather than from the ready notification because enable is a
// request: the child's persisted-port restore can override the asked-for port.
type enableFacadeReply struct {
	Engine string `json:"engine"`
	Port   int    `json:"port"`
}

// codeFacadeBindFailed mirrors the constant of the same name in nvpair-proxy:
// the JSON-RPC error code for an enable that failed only because the port was
// taken. Matching on it is what separates a retryable bind race from a
// rejection no other port would fix.
const codeFacadeBindFailed = -32010

// errFacadeBindFailed tags a bind race locally so callers match on intent
// rather than on a wire code.
var errFacadeBindFailed = stderrors.New("facade bind failed")

// errRPCAnswered tags an enable failure where the child actually replied, as
// opposed to one where the call timed out or the pipe closed.
//
// The distinction decides whether a recorded "ready" from earlier in the same
// attempt can still be trusted: an answered rejection means the child withdrew
// the facade, so that ready is stale. See facadeCameUpAnyway.
var errRPCAnswered = stderrors.New("proxy answered")

// enableProxyFacade asks a proxy process to bring one engine's facade up.
//
// Bounded twice over. proxyCallTimeout is a wedge detector — the child's own
// work is a listen plus two notifications — and parent carries teardown, so
// bring-up is abandoned rather than run to completion while the broker is
// trying to exit. See proxyBringUpContext for why that matters.
func (b *Broker) enableProxyFacade(parent context.Context, p *proxyProcess, spec enableFacadeRequest) error {
	params, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal facade/enable for %s: %w", spec.Engine, err)
	}
	ctx, cancel := context.WithTimeout(parent, proxyCallTimeout)
	defer cancel()

	raw, rpcErr, err := p.Call(ctx, "facade/enable", params)
	if err != nil {
		return fmt.Errorf("facade/enable %s: %w", spec.Engine, err)
	}
	if rpcErr != nil {
		if rpcErr.Code == codeFacadeBindFailed {
			return fmt.Errorf("%w: %w: %s: %s",
				errRPCAnswered, errFacadeBindFailed, spec.Engine, rpcErr.Message)
		}
		return fmt.Errorf("%w: facade/enable %s rejected: %d %s",
			errRPCAnswered, spec.Engine, rpcErr.Code, rpcErr.Message)
	}
	var reply enableFacadeReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		// A malformed reply is still a reply: the child answered and its facade
		// state is whatever it decided, not unknown.
		return fmt.Errorf("%w: decode facade/enable reply for %s: %w",
			errRPCAnswered, spec.Engine, err)
	}
	slog.Info("facade enabled", "engine", reply.Engine, "port", reply.Port,
		"requested", spec.Port)
	return nil
}

// enableProxyFacadeWithFallback enables a facade, retrying once on a fallback
// port if the first attempt lost a bind race. An explicit settings choice is
// never moved automatically; its failure is surfaced to the caller.
//
// Retrying in-process is the point of moving bring-up off argv. A bind failure
// used to end the child so the supervisor could respawn it on a corrected port,
// which is only acceptable while a process hosts one engine: once it hosts
// several, one engine losing a race would take working listeners down with it.
// The supervisor stays the backstop for a second failure.
//
// nextPort receives the port that failed and returns the one to try, or 0 to
// give up. It is a callback because each engine excludes a different set of
// ports — its own engine's port, its alias, its siblings' proxies.
func (b *Broker) enableProxyFacadeWithFallback(
	parent context.Context, p *proxyProcess, spec enableFacadeRequest, nextPort func(failed int) int,
) error {
	err := b.enableProxyFacade(parent, p, spec)
	if err == nil || !stderrors.Is(err, errFacadeBindFailed) {
		return err
	}
	if profile, ok := engineProxyProfileFor(spec.Engine); ok && b.engineProxy(profile).explicitSettings.Load() {
		return err
	}
	// Teardown began between the attempt and the retry: stop here rather than
	// spending another proxyCallTimeout of the shared budget on a port the
	// process is about to stop serving anyway.
	if parent.Err() != nil {
		return err
	}
	failed := spec.Port
	fallback := nextPort(failed)
	if fallback == 0 || fallback == failed {
		return err
	}
	slog.Warn("facade bind failed; retrying on a fallback port",
		"engine", spec.Engine, "port", failed, "fallback", fallback, "err", err)

	spec.Port = fallback
	// The persisted port is what the child would otherwise restore, and it is
	// the port that just failed to bind.
	spec.IgnorePersistedPort = true
	return b.enableProxyFacade(parent, p, spec)
}

// facadeMethodFor strips a facade-scoped notification's engine address and
// confirms it belongs to the engine this reader speaks for.
//
// A process-scoped notification arrives unaddressed and passes through
// unchanged. An addressed one naming a different engine is dropped rather than
// forwarded: forwardProxyProcessNotification already routes by engine, so
// reaching this reader with another engine's address means addressing broke,
// and forwarding it would file one engine's event under another — a wrong port
// on a status, or a cleared error on the wrong tab.
//
// Callers must strip before every dispatch, including the errors relay: a
// prefixed "ollama:errors:report" does not match methodErrorsReport, so an
// unstripped error would fall through to the client stream as an unknown event.
func facadeMethodFor(profile engineProxyProfile, method string) (string, bool) {
	engine, bare := engines.SplitAddressedMethod(method)
	if engine == "" {
		return method, true
	}
	if engine != profile.Name {
		slog.Warn("dropping proxy notification addressed to another engine",
			"reader", profile.Name, "addressed", engine, "method", bare)
		return "", false
	}
	return bare, true
}

// engineProxyProfileForMethod resolves a relayed client method such as
// "ollama-proxy:nodes/list" to the engine whose proxy should receive it.
func engineProxyProfileForMethod(method string) (engineProxyProfile, bool) {
	for _, p := range engineProxyProfiles {
		if strings.HasPrefix(method, p.ComponentName()+":") {
			return p, true
		}
	}
	return engineProxyProfile{}, false
}

// engineProxyProfileFor resolves an engine id.
func engineProxyProfileFor(name string) (engineProxyProfile, bool) {
	for _, p := range engineProxyProfiles {
		if p.Name == name {
			return p, true
		}
	}
	return engineProxyProfile{}, false
}

// proxyEnabled reports whether the broker should front this engine: the
// unified binary has to be resolvable, and the engine has to be one the
// operator asked for via --proxy-engines.
//
// A disabled engine still runs the not-resolved branch at its call site, which
// is what settles its ownership gate. Skipping the branch entirely would leave
// the gate closed and strand every engine request behind it.
func (b *Broker) proxyEnabled(p engineProxyProfile) bool {
	if b.proxyPath == "" {
		return false
	}
	for _, name := range b.proxyEngines {
		if name == p.Name {
			return true
		}
	}
	return false
}

// prepareEnabledFacades prepares managed port ownership for the engines the
// broker is actually going to front, in the table's order — Ollama first,
// because its preparation reserves any inherited OLLAMA_HOST alias that later
// engines must route around.
//
// The enablement check belongs here and not downstream, because preparation is
// not read-only: for a managed engine the backend move runs inside it, so
// preparing an engine whose proxy is never started relocates that engine off
// its own stock port and leaves nothing serving it. Ollama cannot show the
// symptom, since its move is deferred until its proxy proves it holds the
// facade — which is exactly why this cannot be left to the callee.
func (b *Broker) prepareEnabledFacades() {
	if b.proxyEnabled(ollamaProxyProfile) {
		b.prepareManagedOllamaFacade()
	}
	if b.proxyEnabled(lmstudioProxyProfile) {
		b.prepareManagedLMStudioFacade()
	}
}

// proxyDisabledReason explains why an engine has no proxy, and reports whether
// that is worth surfacing to the user rather than only logging it.
//
// The two causes need different treatment: an unresolved binary is a packaging
// or --proxy-path problem someone has to act on, while an engine deliberately
// left out of --proxy-engines is the operator's own choice and an error about
// it would be noise.
func proxyDisabledReason(proxyPath string) (reason string, report bool) {
	if proxyPath == "" {
		return "proxy binary not resolved", true
	}
	return "engine not listed in --proxy-engines", false
}

// addressed prefixes a facade-scoped method with this engine, so a process
// hosting several facades routes it to the right one.
//
// Only facade-scoped methods take an address. facade/enable names its engine in
// the payload, and log/set-level and node/set-priority concern the whole
// process, so addressing any of those would make the child look for a facade
// that the message was never about.
func (p engineProxyProfile) addressed(method string) string {
	return engines.AddressMethod(p.Name, method)
}

// ownershipBlockedID matches the per-engine constants; see
// TestBrokerConstantsMatchTheEngineTable.
func (p engineProxyProfile) ownershipBlockedID() string {
	return p.ComponentName() + ":port-ownership-blocked"
}

// reportProxyUnavailable surfaces an engine the broker is not fronting at all
// because its binary did not resolve.
//
// This shares the ownership-blocked ID so the desktop keeps one entry per
// engine, but it must not reuse that message: no facade was prepared, so
// nothing was reserved and no process was inspected, let alone left running.
// Claiming a failed reservation sends the reader looking for a port conflict
// that does not exist.
func (b *Broker) reportProxyUnavailable(p engineProxyProfile, reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID:        p.ownershipBlockedID(),
		Message:   fmt.Sprintf("NVPAIR is not fronting %s on port %d: %s.", p.DisplayName, p.FacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

// engineProxyHandle returns the live proxy process for an engine, or nil.
func (b *Broker) engineProxyHandle(p engineProxyProfile) *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.engineProxy(p).proxy
}

// setEngineProxyHandle publishes (or clears, with nil) an engine's live proxy.
func (b *Broker) setEngineProxyHandle(p engineProxyProfile, proxy *proxyProcess) {
	b.workersMu.Lock()
	b.engineProxy(p).proxy = proxy
	b.workersMu.Unlock()
}

// forwardEngineProxyNotification is the tail every proxy notification takes
// once the engine-specific handling is done: workload lifecycle and node
// activity are routed into the backend rather than re-emitted, and anything
// else reaches <namespace>:subscribe'd clients as <namespace>:<method>.
//
// This is the end of the line for a notification — every path consumes it, so
// there is nothing for a caller to do afterwards and nothing to report back.
// The heads of the two callers stay separate: the bind-failure and readiness
// handling genuinely differ by ownership, and folding them in behind a
// callback would move those bodies into this file without making them any more
// shared.
func (b *Broker) forwardEngineProxyNotification(profile engineProxyProfile, method string, params json.RawMessage) {
	if b.routeProcessScopedProxyNotification(method, params) {
		return
	}

	b.proxyMu.Lock()
	subscribed := b.engineProxySubscribed(profile)
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify(profile.ComponentName()+":"+method, params); err != nil {
		slog.Warn("forward proxy notification failed", "engine", profile.Name, "method", method, "err", err)
	}
}

// routeProcessScopedProxyNotification handles the notifications that belong to
// the proxy process rather than to one of its facades, and reports whether it
// took the method.
//
// Both routes here read the engine out of the payload — a workload record
// carries its own Engine, and a node-activity report is about a peer — so
// neither needs to know which facade emitted it. That is exactly why they must
// be handled once per process: dispatching them through each engine's handler
// in turn would file every workload twice.
func (b *Broker) routeProcessScopedProxyNotification(method string, params json.RawMessage) bool {
	switch {
	case proxyWorkloadMethods[method]:
		b.routeProxyWorkload(method, params)
		return true
	case method == noderec.NotifyNodeActivity:
		b.routeNodeActivity(params)
		return true
	}
	return false
}

// engineProxySubscribed reports whether a client has subscribed to this
// engine's relay stream. Caller holds proxyMu.
func (b *Broker) engineProxySubscribed(p engineProxyProfile) bool {
	return b.engineProxy(p).subscribed
}

// setEngineProxySubscribed records a subscribe or unsubscribe and reports the
// previous value, which callers use to decide whether to replay a baseline
// snapshot. Caller holds proxyMu.
func (b *Broker) setEngineProxySubscribed(p engineProxyProfile, subscribed bool) bool {
	rt := b.engineProxy(p)
	was := rt.subscribed
	rt.subscribed = subscribed
	return was
}

// relayToEngineProxy forwards an <engine>-proxy:<method> client request to that
// engine's facade and maps the response straight back.
//
// The client's component prefix comes off and the facade address goes on. Both
// name the engine, but they are not the same string and not interchangeable:
// "ollama-proxy:" is how a client addresses the component, "ollama:" is how a
// message addresses a facade inside the process.
//
// <engine>-proxy:shutdown is refused: the broker owns the proxy lifecycle, so a
// client must not be able to kill it out from under us. A missing proxy gets a
// clear error rather than a silent hang.
func (b *Broker) relayToEngineProxy(profile engineProxyProfile, msg *Message) {
	respondErr := func(code int, format string, args ...any) {
		if err := b.codec.RespondError(msg.ID, code, fmt.Sprintf(format, args...)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	}

	method := strings.TrimPrefix(msg.Method, profile.ComponentName()+":")
	if method == "shutdown" {
		respondErr(-32601, "%s:shutdown is not allowed; the broker owns the proxy lifecycle", profile.ComponentName())
		return
	}

	p := b.engineProxyHandle(profile)
	if p == nil {
		respondErr(-32000, "%s not available", profile.ComponentName())
		return
	}

	result, rpcErr, err := p.Call(context.Background(), profile.addressed(method), msg.Params)
	switch {
	case err != nil:
		respondErr(-32000, "proxy call failed: %v", err)
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay %s error for %s: %v", profile.ComponentName(), msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay %s result for %s: %v", profile.ComponentName(), msg.Method, err)
		}
	}
}

// mustEngineProxyProfile resolves an engine id the broker itself names. A miss
// means the shared table and this one disagree, which is a build-time mistake
// rather than a runtime condition, so it panics at init rather than degrading
// into an engine with a zero-valued facade port.
func mustEngineProxyProfile(name string) engineProxyProfile {
	p, ok := engineProxyProfileFor(name)
	if !ok {
		panic("nvpair-ui-broker: no engine proxy profile for " + name)
	}
	return p
}

// planManagedEnginePorts is the ownership policy core, kept separate from RPC
// so every safety branch is deterministic in tests. It never plans a move the
// broker lacks the authority to make: a running engine is repositioned only
// when engine-manager owns it, and an unknown occupant of the facade port is
// never displaced.
//
// The two facade-availability checks below look redundant and are not. An
// adopted engine has to know before it plans anything, because it cannot move
// a running occupant; a managed engine cannot answer the question yet, because
// the likeliest occupant is its own engine, which the relocation branch is
// about to move. The second check is therefore reached only by managed
// engines — for an adopted one the first check has already returned.
func planManagedEnginePorts(p engineProxyProfile, enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	if !enabled {
		return managedPortPlan{}
	}
	facade, backendStart := p.FacadePort, p.EnginePortBase

	// An engine already running on the facade that the broker may not move is
	// the end of the story: it owns the port and there is nothing to plan.
	if !p.mayMoveRunningEngine() && st.Running && (st.Port == facade || st.Port == 0) {
		return managedPortPlan{Blocked: fmt.Sprintf("%s is already running on the compatibility port", p.DisplayName)}
	}
	if p.blocksOnOccupiedFacade() && !available(facade) {
		return managedPortPlan{Blocked: "the compatibility port is already in use"}
	}

	if st.Port > 0 && st.Port != facade {
		// The engine is not on the facade, so an occupant here is a stranger
		// under either ownership.
		if !available(facade) {
			return managedPortPlan{Blocked: "the compatibility port is already in use"}
		}
		// A stopped engine whose configured port has been taken has to be
		// advanced; a running one keeps the port it is serving on.
		if !st.Running && !available(st.Port) {
			backend := nextAvailablePort(backendStart, available)
			if backend == 0 {
				return managedPortPlan{Blocked: "no free backend port is available"}
			}
			return managedPortPlan{Enabled: true, BackendPort: backend}
		}
		return managedPortPlan{Enabled: true}
	}

	backend := nextAvailablePort(backendStart, available)
	if backend == 0 {
		return managedPortPlan{Blocked: "no free backend port is available"}
	}
	return managedPortPlan{Enabled: true, BackendPort: backend}
}
