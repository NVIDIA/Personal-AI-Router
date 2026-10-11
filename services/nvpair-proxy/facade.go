// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/engines"
	"nvpair-shared/errors"
	"nvpair-shared/noderec"
	"nvpair-shared/reach"
	"nvpair-shared/splitlisten"
)

// facade is one engine's presence on this node: the ports it listens on, the
// dialect it speaks, and the routing state that answers for it. Proxy is the
// process that hosts facades and owns everything they share — the JSON-RPC
// codec, the cluster mesh, the outbound transport pool, the scheduler baseline.
//
// One Proxy hosts a facade per enabled engine, keyed by engine id, and they run
// concurrently — see spec.md §3. That is what makes the ownership question
// below a correctness question rather than a stylistic one: a field on the
// wrong side is either duplicated state two engines will disagree about, or
// shared state one engine can corrupt for the other.
//
// Ownership rule for anything added here: a field belongs to the facade if two
// engines would each need their own, and to Proxy if a second copy would be
// wasteful or actively wrong. Listeners are per facade because the ports
// differ. The transport pool is per process because it is keyed by peer, not
// by engine, and duplicating it would double the connections to every peer.
type facade struct {
	// host is the process hosting this facade. Reached for shared state only.
	host *Proxy

	// profile is the engine this facade fronts. Set once at construction and
	// never mutated, so every read is safe without a lock.
	profile engineProfile

	// portMu serializes durable rebinds without blocking routing during storage I/O.
	portMu sync.Mutex

	// httpMu guards port, the servers, the split listener, and the listeners
	// across a live set-port rebind. The HTTP handlers never read port (they
	// route by upstream node), so the only contention is set-port vs set-port
	// (serialized) and the initial serveHTTP store vs a later rebind.
	httpMu       sync.Mutex
	port         int
	aliasAddr    string
	aliasAltAddr string
	plainSrv     *http.Server
	tlsSrv       *http.Server
	split        *splitlisten.Splitter
	aliasLn      net.Listener
	aliasAltLn   net.Listener

	// discovery is this facade's routing overlay: the relay-fed set of peers
	// advertising this engine, plus the manual nodes the broker bridged in.
	// Per facade because the relay filters snapshots by discovery service key,
	// so each engine sees a different set of nodes.
	discovery *Discovery

	// selectedMu guards selectedID, the user's manual route pin. Per facade
	// because pinning a node for Ollama says nothing about LM Studio.
	selectedMu sync.RWMutex
	selectedID string

	// backendMu guards backend, the explicit loopback engine this facade's
	// cluster mTLS ingress forwards to. The broker sets/clears it via
	// node/set-local-backend; it is never sourced from discovery, so an ingress
	// request can only ever reach this node's own local engine and can never be
	// re-routed to a peer.
	backendMu sync.RWMutex
	backend   localBackend

	// targets remembers, per node, which of its published addresses accepted a
	// connection, so a repeated forward costs no confirmation. An entry is
	// re-confirmed when the node's candidate list changes and forgotten on an
	// upstream error, so the next request fails over to another address.
	//
	// Per facade even though it is keyed by node: the candidate addresses carry
	// each engine's own port, so a shared chooser would have two engines
	// overwriting each other's confirmed endpoint for the same peer.
	targets *reach.Chooser

	// nextRequestID tags this facade's RequestStarted / RequestEvent pairs.
	// Atomic add returns the new value, so ids start at 1 per facade.
	//
	// Per facade, and load-bearing: TestWorkloadCrossEngineIdentityDistinct in
	// services/tests requires both engines to mint id "1" concurrently, which
	// is what proves the (origin, engine, runId, id) store key actually
	// separates them. A shared counter would hand the second facade "2" and
	// leave that guarantee untested.
	nextRequestID atomic.Uint64
}

func newFacade(host *Proxy, profile engineProfile, discovery *Discovery, port int) *facade {
	return &facade{
		host:      host,
		profile:   profile,
		discovery: discovery,
		port:      port,
		targets:   reach.NewChooser(),
	}
}

// enableFacadeParams is the broker's request to bring one engine's facade up.
//
// The port arrives here rather than as a process flag because the broker plans
// a different port for each engine, and a single-valued flag cannot carry more
// than one plan. IgnorePersistedPort travels with it for the same reason: it
// qualifies this engine's port, not the process.
type enableFacadeParams struct {
	Engine string `json:"engine"`
	// Port is the port the broker wants this facade on. Zero means the
	// engine's standalone default.
	Port int `json:"port,omitempty"`
	// AliasAddresses are extra loopback-only addresses to answer on, for an
	// engine whose clients read an inherited host variable.
	AliasAddresses []string `json:"aliasAddresses,omitempty"`
	// IgnorePersistedPort uses Port even when this engine has a persisted one.
	IgnorePersistedPort bool `json:"ignorePersistedPort,omitempty"`
}

// facadeWithdrawGrace bounds the teardown of a facade whose bring-up failed.
// Short on purpose: nothing has been announced as serving, so there is no
// legitimate long-lived request to drain.
const facadeWithdrawGrace = 2 * time.Second

// errFacadeBindFailed marks the one enable failure a different port could fix.
// Every other rejection — unknown engine, unsupported alias, port out of range
// — is a caller bug that retrying elsewhere would not help, so the broker must
// be able to tell them apart. It reaches the broker as codeFacadeBindFailed.
var errFacadeBindFailed = stderrors.New("facade bind failed")

// enableFacadeResult reports where the facade actually landed.
//
// The bound port is returned rather than only announced, because the broker
// asks for a port and the persisted-port restore may override it: enable is a
// request, not a command. Reading the outcome from the response lets the caller
// finish its port transaction without waiting for a notification.
type enableFacadeResult struct {
	Engine string `json:"engine"`
	Port   int    `json:"port"`
}

// enableFacade brings up one engine's facade and reports the port it bound.
//
// Enabling an engine that is already up is an idempotent success, so a
// redelivered enable after a timeout does not tear down a working listener.
//
// It deliberately takes no context. The facade's HTTP servers outlive this
// call, and their BaseContext has to be the process lifetime: handing them the
// enable request's context would cancel every request the facade later serves
// the moment this returns.
func (p *Proxy) enableFacade(params enableFacadeParams) (enableFacadeResult, error) {
	profile, ok := profileFor(params.Engine)
	if !ok {
		return enableFacadeResult{}, fmt.Errorf("unknown engine %q; want one of %s", params.Engine, engineNames())
	}
	if params.Port != 0 && (params.Port < 1 || params.Port > 65535) {
		// Zero means "the standalone default", so it is the one out-of-range
		// value with a meaning. Anything else out of range is a caller bug: the
		// listener would bind on an ephemeral port but the facade announces the
		// requested one, leaving a proxy the broker cannot locate.
		return enableFacadeResult{}, fmt.Errorf("port must be between 1 and 65535 (got %d)", params.Port)
	}
	if len(params.AliasAddresses) > 0 && !profile.SupportsHostAlias {
		return enableFacadeResult{}, fmt.Errorf(
			"alias addresses are not supported for engine %q: it has no inherited host variable for the alias to stand in for",
			profile.Name)
	}

	p.facadeMu.Lock()
	defer p.facadeMu.Unlock()

	if existing := p.facades[profile.Name]; existing != nil {
		return enableFacadeResult{Engine: profile.Name, Port: existing.port}, nil
	}

	requested := params.Port
	if requested == 0 {
		requested = profile.StandalonePort
	}
	// Restore a port the user previously chose via set-port, so the facade
	// comes back where they left it rather than where the broker last planned.
	persisted, hasPersisted := loadPersistedPort(profile)
	port := chooseStartupPort(profile, requested, params.IgnorePersistedPort, persisted, hasPersisted)
	if port == persisted {
		slog.Info("restored persisted proxy port", "engine", profile.Name, "port", persisted)
	}

	f := newFacade(p, profile, NewDiscovery(), port)
	for _, address := range params.AliasAddresses {
		if err := f.setLoopbackAlias(address); err != nil {
			return enableFacadeResult{}, fmt.Errorf("invalid alias address %q: %w", address, err)
		}
	}
	// Published before start so a request arriving on this facade's listener,
	// which start opens, finds it.
	if p.facades == nil {
		p.facades = make(map[string]*facade, len(engines.Names()))
	}
	p.facades[profile.Name] = f

	// Withdrawal is deferred rather than done on the error return, because the
	// dispatch recovers panics instead of ending the process: an unwinding
	// bring-up would otherwise leave a published facade that may hold a bound
	// port with no serving loop, and the idempotent branch above would then
	// report that dead port as success to every later enable.
	//
	// Only this engine is withdrawn. Every other facade keeps serving: a lost
	// bind race is one engine's problem, and taking working listeners down with
	// it is exactly what moving off argv was meant to stop.
	started := false
	defer func() {
		if started {
			return
		}
		delete(p.facades, profile.Name)
		// Dispatched to a goroutine, and that is what takes it off the lock:
		// this defer is registered after the facadeMu unlock, so LIFO ordering
		// runs it *before* the unlock, with the mutex still held. Shutting down
		// inline would hold facadeMu for as long as one in-flight request took,
		// blocking every facade lookup and process teardown behind it.
		//
		// Bounded for the same reason it is asynchronous. start releases its
		// own listener when it does not reach serving, so this covers only the
		// window after the split was published.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), facadeWithdrawGrace)
			defer cancel()
			f.stopServing(ctx)
		}()
	}()

	if err := f.start(p.serveCtx); err != nil {
		return enableFacadeResult{}, err
	}
	started = true
	return enableFacadeResult{Engine: profile.Name, Port: f.port}, nil
}

func (f *facade) SelectedID() string {
	f.selectedMu.RLock()
	defer f.selectedMu.RUnlock()
	return f.selectedID
}

func (f *facade) SetSelected(id string) {
	f.selectedMu.Lock()
	defer f.selectedMu.Unlock()
	f.selectedID = id
}

// clearSelectionOf drops the route pin when it points at the given node,
// reporting whether it did so the caller can announce the change.
//
// The lock is released by defer rather than on each branch, so a panic in this
// critical section cannot leave the pin locked. That matters now that a panic
// is recovered instead of ending the process: a stranded mutex would turn a
// contained crash into a hang for every facade.
func (f *facade) clearSelectionOf(id string) bool {
	f.selectedMu.Lock()
	defer f.selectedMu.Unlock()
	if f.selectedID != id {
		return false
	}
	f.selectedID = ""
	return true
}

// clearSelectionIfNotPresent resets the user-selected node (and notifies the
// client) when it's neither in the relay-fed set nor a manual node, so a stale
// selection can't pin routing to a departed target.
func (f *facade) clearSelectionIfNotPresent(present map[string]bool) {
	sel := f.SelectedID()
	if sel == "" || present[sel] || f.discovery.IsManual(sel) {
		return
	}
	// Re-checked under the lock rather than cleared outright: the selection can
	// change while IsManual runs, and clearing a newer pin would drop a choice
	// the user just made.
	if f.clearSelectionOf(sel) {
		_ = f.notify("node/selection-changed", SelectedResult{ID: ""})
	}
}

// targetURL resolves a node to the address this facade should dial.
//
// reach.Prefer, not a blocking confirmation: this runs once per discovered node
// per request, including nodes this request will not be routed to, so a
// handshake here would charge every request for every node's connectivity. An
// address that is wrong is caught by the reverse proxy's ErrorHandler, which
// forgets it and fails over.
func (f *facade) targetURL(n Node) *url.URL {
	candidates := nodeCandidates(n)
	if len(candidates) == 0 {
		return nil
	}
	host := f.targets.Prefer(n.ID, candidates)
	return &url.URL{Scheme: "http", Host: host}
}

// replaceSubscribed replaces this facade's relay-fed routing overlay from a
// discovery:nodes snapshot: it projects every node advertising this engine with
// a dialable IP into the overlay (dropping the rest) and clears a user selection
// pinned to a node that's no longer routable. The broker sends the full filtered
// set on every change, so this is a wholesale replace, not a per-node apply — a
// departed node is simply absent from the next snapshot.
func (f *facade) replaceSubscribed(params json.RawMessage) {
	var res noderec.GetNodesResult
	if err := json.Unmarshal(params, &res); err != nil {
		slog.Warn("invalid discovery:nodes snapshot", "err", err)
		return
	}
	nodes := make([]Node, 0, len(res.Nodes))
	present := make(map[string]bool, len(res.Nodes))
	for _, dn := range res.Nodes {
		n, ok := subscribedToNode(f.profile, dn)
		if !ok {
			continue
		}
		nodes = append(nodes, n)
		present[n.ID] = true
	}
	discovered, updated, removed := f.discovery.SetSubscribed(nodes)
	// Surface the relay-fed set to the client as node/* events — the signal a
	// consumer (the UI) uses to show which peers run this engine — mirroring how
	// manual nodes are announced. Without this the routing overlay updates
	// silently and peers appear engine-less. A node dropping out is also the
	// proxy's "this upstream is gone" signal, surfaced through the errors
	// pipeline (the broker forwards these to nvpair-errors); a re-appearance clears
	// it. NodeID/Timestamp are left unset so the broker stamps the authoritative
	// values.
	for _, n := range discovered {
		_ = f.notify("node/discovered", n.withPrimaryIP())
		if err := f.notify("errors:clear", errors.ClearParams{ID: upstreamUnreachableID(f.profile, n.ID)}); err != nil {
			slog.Debug("failed to send errors:clear", "node", n.ID, "err", err)
		}
	}
	for _, n := range updated {
		_ = f.notify("node/updated", n.withPrimaryIP())
	}
	for _, n := range removed {
		_ = f.notify("node/removed", n.withPrimaryIP())
		if err := f.notify("errors:report", errors.ServiceError{
			ID:       upstreamUnreachableID(f.profile, n.ID),
			Message:  fmt.Sprintf("Upstream node %q is no longer reachable (dropped from discovery)", n.Host),
			Severity: "warning",
			Action:   "none",
		}); err != nil {
			slog.Debug("failed to send errors:report", "node", n.ID, "err", err)
		}
	}
	f.clearSelectionIfNotPresent(present)
}

// notify emits a facade-scoped notification, addressed to this engine.
//
// The address is what lets the broker attribute a "ready" to a facade rather
// than to whichever process the message arrived on. Every facade-scoped
// notification goes through here, so that attribution cannot be forgotten at
// one call site.
//
// Process-scoped notifications — anything the broker attributes to the process
// rather than an engine — go to host.codec directly and must not come here.
func (f *facade) notify(method string, params any) error {
	return f.host.codec.Notify(engines.AddressMethod(f.profile.Name, method), params)
}

// listen binds this facade's TCP listener synchronously so bind failures
// (EADDRINUSE and friends) can be reported through a structured error
// notification before the process exits. The caller is responsible for
// closing the returned listener if it doesn't hand it to serveHTTP.
func (f *facade) listen() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", f.port))
}

// start brings this facade up: bind, reserve the alias, announce readiness,
// subscribe for routing targets, and serve. It is the single bring-up path, so
// there is one place that decides what "the facade is up" means.
//
// The bind is synchronous and precedes the ready announcement. If the port is
// taken we want the reason surfaced, rather than the UI sitting on "proxy
// running" while a background ListenAndServe fails silently.
func (f *facade) start(ctx context.Context) error {
	ln, err := f.listen()
	if err != nil {
		// Best-effort: the caller decides what to do about this, so a failed
		// notify here is not worth surfacing separately.
		_ = f.notify("error", ErrorParams{
			Code:    "bind-failed",
			Message: fmt.Sprintf("failed to bind port %d: %v", f.port, err),
			Port:    f.port,
		})
		// Tagged so the broker can retry this on another port and leave every
		// other enable rejection alone. The cause is kept for the log line.
		return fmt.Errorf("%w: port %d: %w", errFacadeBindFailed, f.port, err)
	}

	// The listener is released unless bring-up completes, and by defer so an
	// unwinding panic releases it too.
	//
	// start owns the port from listen() until serveHTTP hands it to the split.
	// Leaving it held by nothing would be worse than not binding at all: a
	// later enable for this engine gets EADDRINUSE from its own process, the
	// broker reads that as a lost bind race, and the facade spends the rest of
	// the process on a fallback port.
	serving := false
	defer func() {
		if serving {
			return
		}
		_ = ln.Close()
		f.closeLoopbackAlias()
	}()

	// Reserve the inherited OLLAMA_HOST alias before announcing readiness, so a
	// successful ready event is truthful for clients that start the moment
	// NVPAIR comes up. Alias conflicts are deliberately non-fatal: the primary
	// facade stays available and the existing owner is untouched.
	f.bindLoopbackAlias()

	if err := f.notify("ready", ReadyParams{Version: Version, Port: f.port}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	// Routing targets come from the broker's discovery relay. Snapshots arrive
	// as discovery:nodes and each replaces the subscribed overlay. Non-fatal: a
	// parent that is not a relay-aware broker still leaves manual-node routing.
	// Addressed like every other facade-scoped notification, because each
	// facade subscribes to its own engine's discovery service. An unaddressed
	// subscribe would leave the broker holding one subscription id per process,
	// so a second facade's subscribe would silently replace the first's.
	slog.Debug("subscribing to discovery relay for routing targets",
		"service", string(f.profile.DiscoveryService))
	if err := f.notify(noderec.MethodSubscribe, noderec.SubscribeParams{
		Services: []noderec.ServiceKey{f.profile.DiscoveryService},
	}); err != nil {
		slog.Warn("failed to subscribe to discovery relay", "err", err)
	}

	// serveHTTP hands the listener to the split, which stopServing owns from
	// here on, so the deferred release above must stand down.
	f.serveHTTP(ctx, ln)
	serving = true
	f.serveLoopbackAlias()
	return nil
}

// serveHTTP takes the already-bound base listener and drives the two proxy
// personalities over it: a plaintext HTTP server (loopback-only, full local
// router) and a LAN mTLS ingress (pin-gated, forwards to the local engine),
// split by the connection's first byte via nvpair-shared/splitlisten. The two
// http.Servers are recorded so set-port can rebind both onto a fresh split
// without tearing the servers down.
func (f *facade) serveHTTP(ctx context.Context, ln net.Listener) {
	base := func(_ net.Listener) context.Context { return ctx }
	plainSrv := &http.Server{
		Handler:           http.HandlerFunc(f.handlePlain),
		BaseContext:       base,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyServerIdleTimeout,
	}
	tlsSrv := &http.Server{
		Handler:           http.HandlerFunc(f.handleClusterIngress),
		BaseContext:       base,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyServerIdleTimeout,
	}

	f.publishServers(ln, plainSrv, tlsSrv)

	slog.Info("proxy timeouts configured",
		"dial_timeout", proxyDialTimeout,
		"keep_alive", proxyKeepAlive,
		"response_header_timeout", proxyResponseTimeout,
		"max_idle_conns", proxyMaxIdleConns,
		"idle_conn_timeout", proxyIdleConnTimeout,
	)
	slog.Info("HTTP proxy listening", "port", f.port, "addr", ln.Addr().String(),
		"cluster_ingress", f.host.mesh.Clustered())
}

// publishServers records this facade's servers and starts the split listener.
//
// Every httpMu section in this file releases by defer, and that is load-bearing
// rather than stylistic: bring-up is reachable from the JSON-RPC dispatch,
// which recovers panics instead of ending the process. resolveCandidates takes
// httpMu on every request and stopServing takes it during teardown, so a
// stranded httpMu would turn a contained panic into a hung facade plus a hung
// process exit — worse than the crash the recover replaced.
func (f *facade) publishServers(ln net.Listener, plainSrv, tlsSrv *http.Server) {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	f.plainSrv = plainSrv
	f.tlsSrv = tlsSrv
	// The base listener is not retained: the splitter owns it from here, and
	// stopServing closes the splitter. A second copy of the reference was
	// written and never read, which read like teardown released the base
	// listener when in fact only the split ever did.
	f.startSplitLocked(ln)
}

// selfAddresses snapshots this facade's own bound port and alias addresses, so
// the self-forward guard can recognize its own listeners.
func (f *facade) selfAddresses() (selfPort int, aliasBound []string) {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	if f.aliasLn != nil {
		aliasBound = append(aliasBound, f.aliasLn.Addr().String())
	}
	if f.aliasAltLn != nil {
		aliasBound = append(aliasBound, f.aliasAltLn.Addr().String())
	}
	return f.port, aliasBound
}

// servingState snapshots what stopServing has to shut down, so the shutdowns
// themselves run outside httpMu.
func (f *facade) servingState() (plainSrv, tlsSrv *http.Server, split *splitlisten.Splitter, aliasLn, aliasAltLn net.Listener) {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	return f.plainSrv, f.tlsSrv, f.split, f.aliasLn, f.aliasAltLn
}

// aliasAddresses snapshots the configured alias addresses. Index 0 is empty
// when no alias was requested.
func (f *facade) aliasAddresses() []string {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	return []string{f.aliasAddr, f.aliasAltAddr}
}

// setAliasListeners records the reserved alias listeners in order.
func (f *facade) setAliasListeners(listeners []net.Listener) {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	f.aliasLn = listeners[0]
	if len(listeners) > 1 {
		f.aliasAltLn = listeners[1]
	}
}

// aliasServing snapshots what serveLoopbackAlias needs to start its goroutines
// without holding httpMu across the Serve calls.
func (f *facade) aliasServing() (addresses []string, listeners []net.Listener, plainSrv *http.Server) {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	return []string{f.aliasAddr, f.aliasAltAddr},
		[]net.Listener{f.aliasLn, f.aliasAltLn},
		f.plainSrv
}

// takeAliasListeners clears and returns the alias listeners so the caller can
// close them outside the lock.
func (f *facade) takeAliasListeners() []net.Listener {
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	listeners := []net.Listener{f.aliasLn, f.aliasAltLn}
	f.aliasLn = nil
	f.aliasAltLn = nil
	return listeners
}

// setLoopbackAlias validates and records the optional secondary plaintext
// listener. The broker supplies this only for an inherited local HTTP
// OLLAMA_HOST. Validate again here so a direct invocation can never turn the
// alias flag into a LAN plaintext listener.
func (f *facade) setLoopbackAlias(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("expected host:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		host = "127.0.0.1"
		address = net.JoinHostPort(host, portText)
	} else if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q is not loopback", host)
	}
	if port == f.port {
		return fmt.Errorf("alias port %d is already the primary listener", port)
	}
	switch {
	case f.aliasAddr == "":
		f.aliasAddr = address
	case f.aliasAddr == address || f.aliasAltAddr == address:
		return nil
	case f.aliasAltAddr == "":
		_, firstPort, _ := net.SplitHostPort(f.aliasAddr)
		if firstPort != portText {
			return fmt.Errorf("alias addresses must use the same port")
		}
		f.aliasAltAddr = address
	default:
		return fmt.Errorf("at most two alias addresses are supported")
	}
	return nil
}

// bindLoopbackAlias reserves the configured alias before the proxy announces
// ready. A conflict is non-fatal: the primary stays healthy, the existing owner
// is untouched, and a sticky warning tells the user how to recover.
func (f *facade) bindLoopbackAlias() {
	addresses := f.aliasAddresses()
	if addresses[0] == "" {
		return
	}

	listeners := make([]net.Listener, 0, 2)
	for _, address := range addresses {
		if address == "" {
			continue
		}
		ln, err := net.Listen("tcp", address)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			f.reportLoopbackAliasBlocked(address, err)
			return
		}
		listeners = append(listeners, ln)
	}
	f.setAliasListeners(listeners)
	if err := f.notify("errors:clear", errors.ClearParams{ID: ollamaHostAliasBlockedID}); err != nil {
		slog.Debug("failed to clear OLLAMA_HOST alias warning", "err", err)
	}
	slog.Info("OLLAMA_HOST loopback alias reserved", "addresses", addresses)
}

func (f *facade) serveLoopbackAlias() {
	addresses, listeners, plainSrv := f.aliasServing()
	if listeners[0] == nil || plainSrv == nil {
		return
	}
	for i, ln := range listeners {
		if ln == nil {
			continue
		}
		address := addresses[i]
		go func() {
			if err := plainSrv.Serve(ln); err != nil && err != http.ErrServerClosed && !stderrors.Is(err, net.ErrClosed) {
				slog.Error("OLLAMA_HOST alias server exited", "address", address, "err", err)
			}
		}()
	}
	slog.Info("OLLAMA_HOST loopback alias listening", "addresses", addresses)
}

func (f *facade) closeLoopbackAlias() {
	for _, ln := range f.takeAliasListeners() {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

// The alias is gated to engines with SupportsHostAlias, which today is Ollama
// alone, so this prefix is correct by construction rather than by convention —
// enableFacade refuses alias addresses for any other engine. The broker matches
// this exact id.
const ollamaHostAliasBlockedID = "ollama-proxy:ollama-host-alias-blocked"

func (f *facade) reportLoopbackAliasBlocked(address string, bindErr error) {
	message := fmt.Sprintf(
		"NVPAIR kept its primary Ollama compatibility proxy separate, but could not claim the local OLLAMA_HOST alias %s: %v. Stop or reconfigure the application using that port, or change or unset OLLAMA_HOST, then restart NVPAIR. No process was stopped.",
		address, bindErr)
	if err := f.notify("errors:report", errors.ServiceError{
		ID:       ollamaHostAliasBlockedID,
		Message:  message,
		Severity: "warning",
		Action:   "none",
	}); err != nil {
		slog.Debug("failed to report OLLAMA_HOST alias warning", "err", err)
	}
	slog.Warn("OLLAMA_HOST alias unavailable", "address", address, "err", bindErr)
}

// startSplitLocked wraps base in a first-byte splitter and starts both servers
// on its sub-listeners. Caller holds httpMu. Reuses the persistent plainSrv /
// tlsSrv so set-port can call it repeatedly on fresh listeners.
func (f *facade) startSplitLocked(base net.Listener) {
	split := splitlisten.New(base)
	f.split = split
	go func() {
		if err := f.plainSrv.Serve(split.Plain()); err != nil && err != http.ErrServerClosed {
			slog.Error("plaintext HTTP server exited", "err", err)
		}
	}()
	go f.serveTLS(split.TLS())
}

// serveTLS terminates cluster mTLS on the split's TLS sub-listener. The server
// certificate is resolved per handshake from the live mesh, so this one
// sub-listener covers both states: while this node is unclustered there is no
// leaf to present and the handshake is refused (it exposes no LAN inference
// surface), and the moment the node becomes a member the same sub-listener
// serves the pin-gated ingress — no rebind, and no process restart to pick up a
// freshly-minted identity.
func (f *facade) serveTLS(l net.Listener) {
	if err := f.tlsSrv.Serve(tls.NewListener(l, f.host.mesh.ServerTLSConfig())); err != nil && err != http.ErrServerClosed {
		slog.Error("cluster mTLS ingress exited", "err", err)
	}
}

// stopServing gracefully stops this facade's two personalities and closes the
// listeners it owns. It deliberately does not touch the outbound transport
// pool: that is shared across facades, so closing it here would drop another
// engine's pooled connections to every peer. Process teardown owns it.
func (f *facade) stopServing(ctx context.Context) {
	plainSrv, tlsSrv, split, aliasLn, aliasAltLn := f.servingState()
	if plainSrv != nil {
		_ = plainSrv.Shutdown(ctx)
	}
	if tlsSrv != nil {
		_ = tlsSrv.Shutdown(ctx)
	}
	if split != nil {
		_ = split.Close()
	}
	if aliasLn != nil {
		_ = aliasLn.Close()
	}
	if aliasAltLn != nil {
		_ = aliasAltLn.Close()
	}
}

// setPort live-rebinds this facade's HTTP listener onto newPort and persists
// the choice so it survives a restart. It binds the new listener first (so a
// bind failure leaves the current one serving), starts the same server on it,
// then closes the old listener — in-flight connections on the old port drain
// naturally. A fresh `ready` notification announces the new port so the
// orchestrator/UI learn where the proxy is now listening.
// Only the listener swap runs under httpMu. Persisting the choice and
// announcing it happen outside that lock, because every request takes this
// mutex through resolveCandidates: holding it across slow storage or a
// backpressured stdout would stall all inference through this facade, and delay
// teardown with it.
func (f *facade) setPort(newPort int) error {
	f.portMu.Lock()
	defer f.portMu.Unlock()
	swapped, err := f.swapListener(newPort)
	if err != nil || !swapped {
		return err
	}

	if err := f.notify("ready", ReadyParams{Version: Version, Port: newPort}); err != nil {
		slog.Warn("failed to emit ready after rebind", "err", err)
	}
	return nil
}

// swapListener binds newPort and moves both personalities onto it, reporting
// false when the facade was already there. The new listener is bound before the
// old split closes and persisted before the swap, so either failure leaves the
// current listener serving. The caller holds portMu throughout the operation.
func (f *facade) swapListener(newPort int) (bool, error) {
	port, _ := f.selfAddresses()
	if newPort == port {
		return false, savePersistedPort(f.profile, newPort)
	}
	newLn, err := net.Listen("tcp", fmt.Sprintf(":%d", newPort))
	if err != nil {
		return false, fmt.Errorf("failed to bind port %d: %w", newPort, err)
	}
	if err := savePersistedPort(f.profile, newPort); err != nil {
		_ = newLn.Close()
		return false, fmt.Errorf("persist proxy port: %w", err)
	}
	f.httpMu.Lock()
	defer f.httpMu.Unlock()
	oldSplit := f.split
	f.port = newPort

	// Re-serve both personalities on a fresh split over the new listener, then
	// close the old split (and its base listener) so in-flight connections on
	// the old port drain naturally. The plaintext and mTLS personalities always
	// move together as one unit.
	slog.Info("HTTP proxy listening", "port", newPort, "addr", newLn.Addr().String(),
		"cluster_ingress", f.host.mesh.Clustered())
	f.startSplitLocked(newLn)
	if oldSplit != nil {
		_ = oldSplit.Close()
	}
	return true, nil
}
