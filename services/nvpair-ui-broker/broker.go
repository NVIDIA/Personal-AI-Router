// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/engines"
	"nvpair-shared/errors"
	"nvpair-shared/nodeid"
	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"

	"nvpair-ui-broker/relay"
	"nvpair-ui-broker/workloadstore"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

const restoreEnabledEnginesMethod = "engine:restore-enabled"
const prepareEngineShutdownMethod = "engine:prepare-shutdown"

// ReadyParams is the payload of the "app:ready" notification the broker
// emits on every new connection (or once at startup in stdio mode).
// The method name uses the external schema's app: namespace; the
// payload itself remains a single version field.
type ReadyParams struct {
	Version string `json:"version"`
}

// PingResult is the response to the "ping" request. uptime_ms is included
// so callers can sanity-check they're talking to a freshly started broker
// (vs. one that's been running for hours and may be holding stale state
// once we add real state).
type PingResult struct {
	Pong     bool   `json:"pong"`
	Version  string `json:"version"`
	UptimeMS int64  `json:"uptime_ms"`
}

// VersionResult is the response to the "version" request.
type VersionResult struct {
	Version string `json:"version"`
}

// AvailableNode is the per-node wire format used both as the array
// element of the discovery:get-nodes response and as each element of
// the discovery:nodes-changed notification payload. It is intentionally
// narrower than the rich EnrichedNode the broker holds internally —
// only the fields the external schema requires. JSON keys are
// camelCase here because the external consumer's schema demands it;
// the rest of the NVPAIR codebase uses snake_case, so this is the
// deliberate camelCase island at the broker's outer boundary.
type AvailableNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// HostUUID is the node's stable per-host identity (the daemon's uuid= TXT).
	// It is invariant across a PC rename and unique per physical machine, so it
	// is the key clients should dedup/track nodes by; id/name stay the hostname
	// for display. Two machines sharing a hostname are distinct by HostUUID. It
	// is the same value the cluster surface exposes as nodeUuid.
	// Omitted for manual nodes, which carry no UUID and are keyed by id.
	HostUUID  string `json:"hostUuid,omitempty"`
	IPAddress string `json:"ipAddress"`
	// IPAddresses is every address this node published, in its own ranked order
	// with IPAddress first. A multi-homed node has no single address every peer
	// can reach — a direct-connect link is reachable only from the machine on its
	// far end — so a client that must connect walks this list rather than
	// treating IPAddress as the only answer. Omitted when the node published one
	// address, where it would only repeat IPAddress.
	IPAddresses []string `json:"ipAddresses,omitempty"`
	Port        int      `json:"port"`
	LastSeen    int64    `json:"lastSeen"`
	// Trusted is the additive annotation: whether this node is a paired
	// cluster peer (we hold a pin for its cluster-uuid). Populated from the
	// promoted daemon's directory; false for legacy/non-cluster entries.
	Trusted bool `json:"trusted"`
	// Clustered reports whether this node belongs to some cluster (it advertises
	// a cluster-uuid), independent of whether we are paired with it. A client
	// uses it to suppress an invite that an already-clustered peer would reject.
	Clustered bool `json:"clustered,omitempty"`
	// Models is the node's advertised model list, enriched by the daemon from the
	// peer's engine-manager /v1/models endpoint (models-http). Omitted when the
	// node advertises no engines or none are running.
	Models []string `json:"models,omitempty"`
	// ModelsByEngine attributes Models to the engine that serves each model,
	// keyed by engine-manager engine name (e.g. "ollama", "lmstudio"). Additive:
	// Models stays the flat de-duplicated union for consumers that only need the
	// node-level set; this is the per-engine breakdown a consumer needs to show a
	// remote node's models under the correct engine (the UI's engine card).
	// Omitted when no engine reports models.
	ModelsByEngine map[string][]string `json:"modelsByEngine,omitempty"`
	// LoadedByEngine names the models currently resident in memory per engine
	// (normally a subset of ModelsByEngine), enriched by the daemon from the
	// peer's engine-manager /v1/models loadedByEngine field. Lets a per-engine
	// consumer show which of a remote node's models are loaded. Omitted
	// when no engine reports loaded state.
	LoadedByEngine map[string][]string `json:"loadedByEngine,omitempty"`
}

// GetNodesResult is the response to "discovery:get-nodes". Wrapped in an
// object (rather than a bare array) so we can add summary fields later
// without breaking clients. The notification payload uses a bare array
// instead — see emitNodesChanged.
type GetNodesResult struct {
	Nodes []AvailableNode `json:"nodes"`
}

// SubscriptionResult is the ack returned by both "discovery:subscribe"
// and "discovery:unsubscribe". It echoes the resulting subscription
// state so a caller can confirm the broker's view without tracking it
// locally. Both methods are idempotent: re-subscribing or
// re-unsubscribing returns the same state and never errors.
type SubscriptionResult struct {
	Subscribed bool `json:"subscribed"`
}

// ProxyStatusResult is the response to "ollama-proxy:get-status". Ready is false
// (and Port 0) until the supervised ollama-proxy has emitted its "ready"
// notification — or always, if no proxy is being supervised. Clients poll
// this to learn where the local proxy is listening, since the proxy is
// optional and comes up asynchronously after app:ready.
type ProxyStatusResult struct {
	Ready bool `json:"ready"`
	Port  int  `json:"port"`
}

// Broker holds per-session state. A new Broker is constructed for every
// client connection in listen mode so future per-session caches (auth
// tokens, watched-resource cursors, etc.) don't bleed across clients.
type Broker struct {
	// settingsApplyMu serializes engine settings applies end to end, including
	// the engine stop and restart. engineConfigMu guards only the journal and
	// is released while an engine restarts, so this is what keeps two applies —
	// which may compete for the same port — from interleaving. Always take it
	// before engineConfigMu, never the reverse.
	settingsApplyMu      sync.Mutex
	engineConfigMu       sync.Mutex
	engineSettingsLoaded bool
	engineSettingsError  error
	engineSettingsEpoch  string
	engineSettings       map[string]*engineSettingsRecord
	settingsRelayMu      sync.Mutex
	settingsCancels      map[string]context.CancelFunc
	activeSettings       map[string]activeSettingsOperation
	codec                *Codec
	cancel               context.CancelFunc
	startedAt            time.Time
	nodeID               string
	scannerPath          string
	nodeInfoPath         string
	proxyPath            string
	proxyEngines         []string
	workloadMgrPath      string
	errorsPath           string
	engineMgrPath        string
	manualNodesPath      string
	settingsPath         string
	clusterMgrPath       string
	schedulerPath        string
	clusterDir           string
	// Managed-port state is prepared before proxy startup and read by the proxy
	// supervisor/reader goroutines. Ollama commits its pending backend move after
	// its proxy reserves :11434; LM Studio moves through engine-manager first,
	// then releases its gate after the proxy owns :1234 and has its backend.
	//
	// managedFacade, backendPort and startupPort now live per engine on
	// engineProxyRuntime. What remains here is Ollama's deferred-move state,
	// which only an adopted engine has.
	managedOllamaBackend atomic.Int32
	ollamaMoveInFlight   atomic.Bool
	// ollamaHostAlias is one synchronized snapshot because engine-manager and
	// proxy supervisors can respawn while startup is still committing (or
	// rolling back) the inherited, local-http OLLAMA_HOST alias.
	ollamaHostAliasMu                sync.RWMutex
	ollamaHostAlias                  ollamaHostAlias
	ollamaProxyGeneration            uint64 // guarded by ollamaHostAliasMu
	ollamaHostAliasSyncMu            sync.Mutex
	ollamaHostAliasErrorMu           sync.Mutex
	ollamaHostAliasError             *errors.ServiceError
	ollamaPortReady                  chan struct{}
	ollamaPortReadyOnce              sync.Once
	lmstudioProxyGeneration          atomic.Uint64
	lmstudioProxyPublishedGeneration atomic.Uint64
	lmstudioPortReady                chan struct{}
	lmstudioPortReadyOnce            sync.Once
	lmstudioReadyMu                  sync.Mutex
	store                            *discoveryStore
	telemetry                        *telemetryCache
	// relayDir is the discovery directory, fed by the promoted daemon's
	// discovery:node-* stream. It runs alongside the legacy store during the
	// migration; the client-facing get-nodes/subscribe path moves onto it at the
	// flag-day cutover.
	relayDir *relay.Directory
	// regCache holds this node's service registrations. The broker owns
	// registration for the workers it spawns (it knows their ports; transport is
	// policy-derived), relaying the set to the daemon and replaying it whenever
	// the daemon (re)starts.
	regCache *relay.RegistrationCache

	// workersMu guards the supervised worker handles below. They are no
	// longer assigned once in Serve and then treated as read-only: each
	// worker's supervisor swaps its handle on a restart (on the monitor
	// goroutine) while request handlers and the log-level fan-out read
	// them (on the read-loop / reader goroutines). Every access therefore
	// goes through the get*/set* helpers under this lock.
	workersMu sync.Mutex
	// serveCtx is the broker's own lifetime, recorded so work started on a
	// spawn goroutine can be abandoned when the broker is shutting down.
	// Guarded by workersMu because a respawn reads it off the monitor
	// goroutine. See proxyBringUpContext.
	serveCtx    context.Context
	scanner     *scannerProcess
	nodeInfo    *nodeInfoProcess
	workloadMgr *workloadManagerProcess
	errorsProc  *errorsProcess
	engineMgr   *rpcWorker
	manualNodes *rpcWorker
	settings    *rpcWorker
	clusterMgr  *clusterManagerProcess
	scheduler   *rpcWorker

	// Per-worker supervisors own the (re)spawn + crash-detect + restart
	// lifecycle of each worker. nil when the worker's binary wasn't resolved
	// (or its first spawn failed).
	//
	// proxySup covers every engine: one nvpair-proxy process hosts a facade per
	// enabled engine, so there is no per-engine proxy supervisor.
	scannerSup     *supervisor
	nodeInfoSup    *supervisor
	proxySup       *supervisor
	workloadMgrSup *supervisor
	errorsSup      *supervisor
	engineMgrSup   *supervisor
	manualNodesSup *supervisor
	settingsSup    *supervisor
	clusterMgrSup  *supervisor
	schedulerSup   *supervisor

	// subMu guards subscribed. The discovery:nodes-changed stream is
	// opt-in: emitNodesChanged (called on the scanner-event goroutine)
	// reads this flag while the discovery:subscribe / discovery:unsubscribe
	// handlers (called on the read-loop goroutine) flip it, so the two
	// goroutines need a lock between them.
	subMu      sync.Mutex
	subscribed bool

	// proxyMu guards every engineProxyRuntime.subscribed. The
	// <namespace>:<event> streams are opt-in like discovery's: the forward
	// hooks (on each proxy's reader goroutine) read the flag while the
	// subscribe / unsubscribe handlers (on the read-loop goroutine) flip it.
	proxyMu sync.Mutex

	// engineProxies holds per-engine proxy state, one entry per engine in the
	// descriptor table. Built on first use; see engineProxy.
	engineProxiesOnce sync.Once
	engineProxies     map[string]*engineProxyRuntime

	// workloadsMu guards workloadsSubscribed. The workloads:* stream is
	// opt-in too: emitWorkloadEvent (called on the proxy reader goroutine
	// for local echoes and on the workload-manager reader goroutine for
	// peer-origin relays) reads the flag while the workloads:subscribe /
	// workloads:unsubscribe handlers (on the read-loop goroutine) flip it.
	workloadsMu         sync.Mutex
	workloadsSubscribed bool

	// workloadEmitMu serializes the apply→fan→notify sequence in
	// emitWorkloadEvent so the client-visible order matches the order events
	// are applied to the store, even when the node-loss sweep (a separate
	// goroutine) races a relayed event for the same workload.
	workloadEmitMu sync.Mutex

	// workloads is the broker's authoritative index of cluster workloads
	// (current + historic), keyed by (originatedFrom, id). It applies an
	// order-independent, monotonic merge so a stale/out-of-order event can't
	// resurrect a finished workload, and it backs the node-loss sweep
	// (failWorkloadsForNode). Written from the proxy / workload-manager reader
	// goroutines (via emitWorkloadEvent) and the scanner-event goroutine (via
	// failWorkloadsForNode); it is internally synchronized.
	workloads *workloadstore.Store

	// engineMu guards engineSubscribed. The engine:<event> stream is
	// opt-in like proxy's: forwardEngineNotification (engine-manager reader
	// goroutine) reads the flag while the engine:subscribe /
	// engine:unsubscribe handlers (read-loop goroutine) flip it.
	engineMu         sync.Mutex
	engineSubscribed bool

	// engineRelayMu guards engineRelaySubID, the relay-directory subscription
	// id for engine-manager's ec peer set (its upward discovery:subscribe). A
	// (re)subscribe replaces the prior registration; worker exit clears it.
	engineRelayMu    sync.Mutex
	engineRelaySubID int

	// manualMu guards manualNodeKeys, mapping each manual node's id to the
	// discovery-store key it currently occupies — its real hostUuid once
	// node-info reports one, else its manual id until then. Tracked
	// per manual id so the broker can (a) re-key the store entry when a manual
	// node's node-info first reveals its stable UUID, and (b) evict the right
	// key on removal or a manual-nodes crash — since the store key is no longer
	// the manual id once the UUID is known. Several manual aliases (distinct
	// names/addresses for one machine) can map to the same key; the store/proxy
	// manual claim is dropped only once the last alias for a key is gone, and
	// when an alias that a key was currently projecting from leaves, a surviving
	// alias is reprojected from manualNodeStatuses (its last-seen payload) so the
	// removed alias's address/models don't linger (the survivor's own periodic
	// probe emits no update while its status is unchanged).
	manualMu           sync.Mutex
	manualNodeKeys     map[string]string
	manualNodeStatuses map[string]manualNodeStatusEntry

	// schedMu guards the cached ranking and its generation.
	//
	// One cache, not one per engine: the scheduler computes a single node-wide
	// ranking and emits it once per engine, so per-engine copies would hold
	// identical content and prevent the duplicate from being recognized as one.
	// havePriority distinguishes "no ranking yet" from a cached empty one, which
	// is a legitimate state meaning the scheduler has no influence.
	//
	// priorityApplyMu serializes asynchronous node/set-priority deliveries; a
	// stale generation is skipped before it can overwrite a newer proxy order.
	schedMu            sync.Mutex
	lastPriority       schedulerwire.Priority
	havePriority       bool
	priorityGeneration uint64
	priorityApplyMu    sync.Mutex

	// schedulerFeedMu serializes scheduler initialization with live discovery
	// and workload fanout. A restarted scheduler receives its active workload
	// baseline before its discovery baseline, then only newer live events.
	schedulerFeedMu sync.Mutex
}

// workerPaths bundles the resolved binary paths for every worker the
// broker can supervise. Carrying them as a struct keeps NewBroker's
// signature stable as more workers are adopted. The scanner path is
// required; every other field is optional (an empty string means the
// broker won't spawn that worker because its binary couldn't be resolved).
type workerPaths struct {
	scanner  string
	nodeInfo string
	// proxy is the single nvpair-proxy binary, started once per entry in
	// proxyEngines and then asked for that engine's facade via facade/enable.
	proxy        string
	proxyEngines []string
	workloadMgr  string
	errors       string
	engineMgr    string
	manualNodes  string
	settings     string
	clusterMgr   string
	scheduler    string
	// clusterDir is the cluster-manager config dir (node.crt/node.key +
	// trusted/). Threaded to every worker that does cluster-scoped inter-node
	// mTLS so they serve/dial pinned peers once this node joins a cluster.
	clusterDir string
}

// NewBroker constructs a per-session broker. paths.scanner is required —
// the scanner is the broker's core worker. Every other path is optional:
// an empty string means the broker won't spawn that worker (e.g. the
// binary couldn't be resolved). Without node-info the broker runs without
// local node advertisement; without the proxy, no local Ollama reverse
// proxy; without the workload-manager, no cluster workload relay; without
// nvpair-errors, the service-error pipeline is disabled (producers' errors
// are dropped, as before); without the cluster-manager, no cluster pairing
// and membership (cluster:* / nodes:*). Discovery still works in every case.
func NewBroker(codec *Codec, paths workerPaths) *Broker {
	// Resolve the local node id once, as the stable per-host UUID (the same
	// value the node-scanner advertises as hostUuid and the cluster-manager
	// uses as nodeUuid). It's stamped onto every local-origin workload
	// (originatedFrom) and every local error (nodeId) so the whole cluster keys
	// this node by an identity that survives a PC rename — hostname is display
	// only. The same value is passed to nvpair-errors via --node-id so
	// its localNodeID stays in lockstep with what the broker stamps.
	nodeID := resolveLocalNodeID(paths.clusterDir)
	return &Broker{
		codec:              codec,
		startedAt:          time.Now(),
		nodeID:             nodeID,
		scannerPath:        paths.scanner,
		nodeInfoPath:       paths.nodeInfo,
		proxyPath:          paths.proxy,
		proxyEngines:       paths.proxyEngines,
		workloadMgrPath:    paths.workloadMgr,
		errorsPath:         paths.errors,
		engineMgrPath:      paths.engineMgr,
		manualNodesPath:    paths.manualNodes,
		settingsPath:       paths.settings,
		clusterMgrPath:     paths.clusterMgr,
		schedulerPath:      paths.scheduler,
		clusterDir:         paths.clusterDir,
		store:              newDiscoveryStore(),
		telemetry:          newTelemetryCache(),
		relayDir:           relay.NewDirectory(),
		regCache:           relay.NewRegistrationCache(),
		manualNodeKeys:     make(map[string]string),
		manualNodeStatuses: make(map[string]manualNodeStatusEntry),
		workloads:          workloadstore.New(),
		ollamaPortReady:    make(chan struct{}),
		lmstudioPortReady:  make(chan struct{}),
	}
}

// registerService records a local service in the discovery registration cache
// and relays it to the promoted daemon so it's advertised on this node's
// _nvpair-node record. Idempotent — only a real change is pushed. Broker-local:
// the broker registers on behalf of the workers it spawns.
func (b *Broker) registerService(p noderec.RegisterParams) {
	if !b.regCache.Register(p) {
		return
	}
	if sc := b.getScanner(); sc != nil {
		go sc.pushRegister(p)
	}
}

// unregisterService removes a local service from the cache and the daemon.
func (b *Broker) unregisterService(svc noderec.ServiceKey) {
	if !b.regCache.Unregister(svc) {
		return
	}
	if sc := b.getScanner(); sc != nil {
		go sc.pushUnregister(svc)
	}
}

// get*/set* are the workersMu-guarded accessors for the supervised worker
// handles. A supervisor swaps a handle on the monitor goroutine when it
// restarts a worker, so request handlers and the log-level fan-out must
// read through these rather than touching the fields directly.
func (b *Broker) setScanner(s *scannerProcess) {
	b.workersMu.Lock()
	b.scanner = s
	b.workersMu.Unlock()
}

func (b *Broker) getScanner() *scannerProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.scanner
}

func (b *Broker) setNodeInfo(n *nodeInfoProcess) {
	b.workersMu.Lock()
	b.nodeInfo = n
	b.workersMu.Unlock()
}

func (b *Broker) getNodeInfo() *nodeInfoProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.nodeInfo
}

func (b *Broker) setProxy(p *proxyProcess) {
	b.setEngineProxyHandle(ollamaProxyProfile, p)
}

func (b *Broker) getProxy() *proxyProcess {
	return b.engineProxyHandle(ollamaProxyProfile)
}

func (b *Broker) setWorkloadMgr(m *workloadManagerProcess) {
	b.workersMu.Lock()
	b.workloadMgr = m
	b.workersMu.Unlock()
}

func (b *Broker) getWorkloadMgr() *workloadManagerProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.workloadMgr
}

func (b *Broker) setErrors(e *errorsProcess) {
	b.workersMu.Lock()
	b.errorsProc = e
	b.workersMu.Unlock()
}

func (b *Broker) getErrors() *errorsProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.errorsProc
}

func (b *Broker) setEngineMgr(w *rpcWorker) {
	b.workersMu.Lock()
	b.engineMgr = w
	b.workersMu.Unlock()
}

func (b *Broker) getEngineMgr() *rpcWorker {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.engineMgr
}

func (b *Broker) restoreEnabledEngines(w *rpcWorker) {
	if w == nil {
		return
	}
	// An unreadable journal must not discard the component stores' enabled
	// intent. Recovery logs the failure; restore still uses the saved runtime.
	b.recoverEngineSettings()
	if err := w.Notify(restoreEnabledEnginesMethod, nil); err != nil {
		slog.Warn("failed to request enabled-engine restoration", "err", err)
	}
}

func (b *Broker) restoreEnabledEnginesAfterPortGate(ctx context.Context) bool {
	if !b.waitForManagedPortOwnership(ctx) {
		return false
	}
	b.restoreEnabledEngines(b.getEngineMgr())
	return true
}

func (b *Broker) runEngineAvailabilityAfterPortGates(
	ctx context.Context,
	runOllama func(context.Context),
	runLMStudio func(context.Context),
) bool {
	if !b.restoreEnabledEnginesAfterPortGate(ctx) {
		return false
	}
	go runOllama(ctx)
	runLMStudio(ctx)
	return true
}

func (b *Broker) setManualNodes(w *rpcWorker) {
	b.workersMu.Lock()
	b.manualNodes = w
	b.workersMu.Unlock()
}

func (b *Broker) getManualNodes() *rpcWorker {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.manualNodes
}

func (b *Broker) setSettings(w *rpcWorker) {
	b.workersMu.Lock()
	b.settings = w
	b.workersMu.Unlock()
}

func (b *Broker) getSettings() *rpcWorker {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.settings
}

func (b *Broker) setScheduler(w *rpcWorker) {
	b.workersMu.Lock()
	b.scheduler = w
	b.workersMu.Unlock()
}

func (b *Broker) getScheduler() *rpcWorker {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.scheduler
}

func (b *Broker) setClusterMgr(c *clusterManagerProcess) {
	b.workersMu.Lock()
	b.clusterMgr = c
	b.workersMu.Unlock()
}

func (b *Broker) getClusterMgr() *clusterManagerProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.clusterMgr
}

// spawnScanner / spawnNodeInfo / spawnProxy / spawnWorkloadManager are the
// per-worker spawn closures the supervisor calls for the initial start and
// every restart. Each starts a fresh process (which wires its own reader
// goroutines), stores the concrete handle on the Broker under workersMu,
// and returns it as a supervisedHandle for the supervisor to watch. A
// spawn failure returns the error without storing a handle, leaving the
// previous (dead) handle in place until the next successful (re)start.
func (b *Broker) spawnScanner() (supervisedHandle, error) {
	sp, err := startScanner(
		b.scannerPath,
		applog.LevelString(),
		b.store,
		b.relayDir,
		b.failWorkloadsForNode,
		func(telemetry noderec.NodeTelemetry) { b.ingestTelemetry(sourceScanner, telemetry) },
		func(hostUUID string) { b.removeTelemetry(sourceScanner, hostUUID) },
		b.clusterDirArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setScanner(sp)
	// Replay cached registrations to the (re)started daemon so it re-advertises
	// this node's services without any worker reconnect logic (epoch replay).
	for _, p := range b.regCache.Snapshot() {
		go sp.pushRegister(p)
	}
	slog.Info("scanner started", "path", b.scannerPath, "pid", sp.cmd.Process.Pid)
	return sp, nil
}

func (b *Broker) spawnNodeInfo() (supervisedHandle, error) {
	// Deliberately NOT passed b.clusterDirArgs(): node-info stays plain HTTP even
	// when clustered, and is the ONE documented exception to "the inter-node
	// cluster data plane is always mTLS" (errors and workload-manager admit only
	// pinned members, in either direction). Its GPU/CPU/memory inventory is the
	// lowest-sensitivity inter-node surface and is read directly by consumers that
	// hold no cluster identity — the desktop app's two-second telemetry poll and
	// baseline LAN discovery — so gating it would remove node telemetry from the UI.
	// The accepted risk and its compensating controls are recorded under
	// "Documented Risk Acceptances" in desktop/SECURITY.md.
	//
	// node-info still implements the cluster-gated mode (pass it --cluster-dir and
	// it admits only pinned peers). It is kept, unenabled, on purpose: it is the
	// swap point if the desktop telemetry read is ever solved another way. Enabling
	// it here without solving that first WILL blank the UI's node cards.
	//
	// Do pass --node-id: node-info reports the broker's already-resolved UUID so
	// /v1/node-info agrees with the identity the fleet keys on, even on a custom
	// --cluster-dir data root (node-info gets no cluster-dir, so it would
	// otherwise resolve the default root and could report a different UUID).
	np, err := startNodeInfo(b.nodeInfoPath, applog.LevelString(), b.forwardNodeInfoNotification, "--node-id", b.nodeID)
	if err != nil {
		return nil, err
	}
	b.setNodeInfo(np)
	// Tell it our membership: node-info reports the cluster principal on
	// /v1/node-info but holds no cluster dir to read it from, so this push is the
	// only source. It runs on every spawn, which also covers a supervised restart.
	b.pushClusterIdentityToNodeInfo()
	// Register node-info's service so the daemon advertises ni= on _nvpair-node.
	// node-info binds the fixed :14318 (force_ports is inert), so the broker
	// knows its port. Idempotent across restarts.
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: nodeInfoHTTPPort})
	slog.Info("node-info started", "path", b.nodeInfoPath, "pid", np.cmd.Process.Pid)
	return np, nil
}

// proxyArgs builds the child's argv. Only process-scoped flags remain: which
// engines it fronts, on which ports, and with which alias addresses all travel
// in facade/enable, because the broker plans a different port per engine and a
// single-valued flag cannot carry two plans.
func (b *Broker) proxyArgs() []string {
	// Thread the cluster dir so the proxy can bring up its pin-gated LAN mTLS
	// ingress (and dial peers over mTLS) once this node is clustered; empty/
	// absent certs leave it loopback-plaintext only.
	return b.clusterDirArgs()
}

// ollamaFacadeSpec is the enable request for Ollama's facade under one alias
// snapshot. Taken as a parameter rather than read here so the caller keeps the
// generation bump — beginOllamaProxyGeneration has a side effect — and so the
// spec is assertable on its own.
func (b *Broker) ollamaFacadeSpec(alias ollamaHostAlias) enableFacadeRequest {
	spec := enableFacadeRequest{Engine: ollamaProxyProfile.Name}
	if port := int(b.ollamaState().startupPort.Load()); port != 0 {
		spec.Port = port
		spec.IgnorePersistedPort = true
	}
	if alias.Address != "" {
		spec.AliasAddresses = append(spec.AliasAddresses, alias.Address)
	}
	if alias.AlternateAddress != "" {
		spec.AliasAddresses = append(spec.AliasAddresses, alias.AlternateAddress)
	}
	return spec
}

// setServeCtx records the broker's own lifetime so work started on a spawn
// goroutine can be abandoned when the broker is shutting down.
func (b *Broker) setServeCtx(ctx context.Context) {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	b.serveCtx = ctx
}

func (b *Broker) getServeCtx() context.Context {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.serveCtx
}

// proxyBringUpContext bounds facade bring-up and abandons it the moment either
// the broker or the proxy supervisor starts shutting down.
//
// This exists because spawn stopped being instantaneous. It used to fork a
// process and return; it now issues facade/enable RPCs — up to two per engine,
// each bounded by proxyCallTimeout — and supervisor.Stop blocks on the spawn
// returning with no timeout of its own. A crash-looping proxy whose fresh child
// never answers could therefore spend the whole teardownBudget inside the
// proxy's join, and graceFor would clip every worker behind it, including
// engine-manager's stop grace. stopwait.go records that cutting that grace
// short is what orphaned engines.
//
// Both signals are needed, not either one. Stopping() covers a respawn, but the
// *first* spawn runs synchronously inside Start on the Serve goroutine — Serve
// cannot reach shutdownInferenceStack to call Stop until that spawn returns, so
// Stopping() can never close during it. The Serve context is what interrupts
// that case.
//
// The caller must cancel, as with any context: shutdown is one way out, a
// completed bring-up is the other.
func (b *Broker) proxyBringUpContext() (context.Context, context.CancelFunc) {
	parent := b.getServeCtx()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	sup := b.proxySup
	if sup == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-sup.Stopping():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// enableEngineFacade brings up one engine's facade with that engine's port plan
// and fallback policy. The alias snapshot is only meaningful to an engine with
// an inherited host variable, so the others ignore it.
func (b *Broker) enableEngineFacade(
	ctx context.Context, pp *proxyProcess, profile engineProxyProfile, alias ollamaHostAlias,
) error {
	switch profile.Name {
	case ollamaProxyProfile.Name:
		return b.enableProxyFacadeWithFallback(ctx, pp, b.ollamaFacadeSpec(alias), b.ollamaFallbackPort)
	case lmstudioProxyProfile.Name:
		return b.enableProxyFacadeWithFallback(ctx, pp, b.lmstudioFacadeSpec(), b.lmstudioFallbackPort)
	default:
		return fmt.Errorf("no facade spec for engine %q", profile.Name)
	}
}

// blockAndFinishEngineProxy gives up one engine's managed claim, reports it, and
// releases its startup gate — the treatment for an engine whose facade will not
// come up in a process that is otherwise staying.
//
// The block and the finish are both needed. Blocking gives up the compatibility
// port and plans a fallback but deliberately leaves the OLLAMA_HOST alias
// reserved, because most of its callers still have a live facade serving that
// alias. Finishing is what returns the alias, which is only correct once this
// engine is known not to be coming up.
func (b *Broker) blockAndFinishEngineProxy(profile engineProxyProfile) {
	switch profile.Name {
	case ollamaProxyProfile.Name:
		if b.ollamaState().managedFacade.Load() {
			b.blockManagedOllamaFacade("the Ollama proxy facade could not be brought up")
		}
		b.finishOllamaProxyTerminal()
	case lmstudioProxyProfile.Name:
		if b.lmstudioState().managedFacade.Load() {
			_, _ = b.blockManagedLMStudioFacade("the LM Studio proxy facade could not be brought up", nil)
		}
		b.finishLMStudioProxyTerminal()
	default:
		slog.Warn("no terminal handling for engine", "engine", profile.Name)
	}
}

// facadeCameUpAnyway reports whether a facade that failed to answer its enable
// nevertheless announced itself ready.
//
// Only for **transport-level ambiguity** — a timeout or a lost response, where
// the child does the bind, two notification writes and the serve inside that
// call and may genuinely be serving. Disowning it then would strand a listener
// on a port the broker reports as unavailable, with no facade/disable to
// withdraw it, so the facade's own ready notification gets the deciding vote.
//
// Deliberately not applied to an explicit rejection. A rejection means the
// child decided against the facade and withdrew it, and a recorded ready from
// earlier in the same attempt is then stale: honouring it would publish a dead
// port and handle, and the broker would never retry because it believes the
// facade is up. errFacadeBindFailed is the clearest case — the child deleted
// the facade before answering — but the rule holds for any answered error.
func facadeCameUpAnyway(pp *proxyProcess, engine string, enableErr error) bool {
	if !isTransportAmbiguous(enableErr) {
		return false
	}
	ready, port := pp.Status(engine)
	if !ready || port <= 0 {
		return false
	}
	slog.Warn("facade enable did not answer, but the facade reported ready; keeping it",
		"engine", engine, "port", port, "err", enableErr)
	return true
}

// isTransportAmbiguous reports whether an enable failure leaves the child's
// actual state unknown, as opposed to the child having answered.
//
// errRPCAnswered wraps every case where a response arrived, so the absence of
// it is what identifies a timeout, a closed pipe, or a cancelled call.
func isTransportAmbiguous(err error) bool {
	return err != nil && !stderrors.Is(err, errRPCAnswered)
}

// ollamaFallbackPort is the port to retry an Ollama facade on after a lost bind
// race.
//
// The child's bind-failed notification precedes the enable response on the same
// stream, and forwardProxyNotification handles it inline — so by the time the
// response is dispatched, that path has usually already chosen a fallback and
// reported the ownership block to the UI. Prefer its choice: it also decides
// whether the managed facade is off, which a port picked here would not.
//
// Recomputed only when that did not happen, because the notification is
// best-effort on the child side.
func (b *Broker) ollamaFallbackPort(failed int) int {
	if planned := int(b.ollamaState().startupPort.Load()); planned != 0 && planned != failed {
		return planned
	}
	return b.setOllamaProxyFallback(failed)
}

// spawnProxy starts the one nvpair-proxy process and brings up a facade in it
// for every enabled engine.
//
// One process for all of them is what makes the scheduler's picture whole: the
// estimated-work reservations a proxy takes between scheduler snapshots live in
// the process, so two processes each kept half the story and could send
// simultaneous bursts to the same node believing it idle.
//
// A facade that fails to come up is logged and skipped, not fatal. The spawn
// only fails if no engine came up at all, because a process with no listener
// serves nobody and should be retried by the supervisor rather than left
// running.
func (b *Broker) spawnProxy() (supervisedHandle, error) {
	// Both generations are captured before the spawn and advance together: they
	// are per-incarnation stale-notification filters, and with one process the
	// facades are re-created as a set. Each engine keeps its own counter so its
	// existing port choreography is untouched.
	ollamaGeneration, alias := b.beginOllamaProxyGeneration()
	// Bumped without lmstudioReadyMu. Reconciliation holds that mutex across
	// its set-port and engine:status round trips, so taking it here would make
	// a replacement spawn wait out the dead process's RPC timeouts before it
	// could even start. The generation itself is atomic, and
	// lmstudioProxyGenerationIsCurrent is what drops the stale run.
	lmstudioGeneration := b.lmstudioProxyGeneration.Add(1)

	pp, err := startProxy(engines.ProxyComponent, b.proxyPath, applog.LevelString(), b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardProxyProcessNotification(ollamaGeneration, lmstudioGeneration, method, params)
		}, b.proxyArgs()...)
	if err != nil {
		return nil, err
	}
	// Published for every engine before any facade is enabled, so a facade's
	// own ready notification finds the handle it belongs to.
	for _, profile := range engineProxyProfiles {
		if b.proxyEnabled(profile) {
			b.setEngineProxyHandle(profile, pp)
		}
	}
	b.lmstudioProxyPublishedGeneration.Store(lmstudioGeneration)
	slog.Info("proxy started", "path", b.proxyPath, "pid", pp.cmd.Process.Pid)

	// The child is serving its JSON-RPC channel but has no listeners yet, so
	// ask for each facade before reporting the worker up. Synchronous, on the
	// spawn goroutine, so a caller that treats spawn success as "the engines
	// are being brought up" stays correct.
	bringUp, cancelBringUp := b.proxyBringUpContext()
	defer cancelBringUp()

	enabled := 0
	var failed []engineProxyProfile
	for _, profile := range engineProxyProfiles {
		if !b.proxyEnabled(profile) {
			continue
		}
		err := b.enableEngineFacade(bringUp, pp, profile, alias)
		switch {
		case err == nil:
			enabled++
		case facadeCameUpAnyway(pp, profile.Name, err):
			enabled++
		default:
			slog.Warn("facade enable failed; continuing without a local proxy for it",
				"engine", profile.Name, "err", err)
			failed = append(failed, profile)
		}
	}
	if enabled == 0 {
		// The whole spawn failed, so the supervisor will retry it. Leave every
		// engine's managed claim and its OLLAMA_HOST alias reservation exactly
		// as they are: the retry needs them, and releasing the alias here would
		// let engine-manager take that port in between, so a retry that would
		// otherwise have succeeded comes up without the inherited endpoint.
		//
		// A first-spawn failure and an exhausted restart budget are the paths
		// that give up for good, and the supervisor owns both.
		pp.Stop()
		for _, profile := range engineProxyProfiles {
			b.setEngineProxyHandle(profile, nil)
		}
		// The startup gates open anyway, because they are a separate concern
		// from the claim: the alias reservation has to survive a retry, the
		// gates must not wait for one. waitForManagedPortOwnership is unbounded
		// apart from the Serve context and the gates also hold client
		// requests, so keeping them shut across a 1→16s backoff budget defers
		// desired-state restore and makes every engine:status wait out its call
		// timeout and answer "retry" — the exact symptom finishOllamaProxyTerminal
		// exists to prevent. A later successful spawn re-derives the real state.
		b.markOllamaPortReady()
		b.markLMStudioPortReady()
		return nil, fmt.Errorf("no engine facade could be brought up in %s", b.proxyPath)
	}

	// At least one facade is serving, so this process incarnation is staying
	// and will not be retried. Only now is a failed engine terminal for it.
	for _, profile := range failed {
		b.setEngineProxyHandle(profile, nil)
		b.blockAndFinishEngineProxy(profile)
	}

	// A facade can announce ready before its handle was published, so replay
	// the reconciliation now. Both gates are sync.Once-guarded, which makes
	// this safe when the notification goroutine already handled it.
	if b.ollamaState().managedFacade.Load() {
		if ready, port := pp.Status(ollamaProxyProfile.Name); ready && port > 0 {
			go b.reconcileProxyPortOnReady(port)
		}
	}
	if ready, port := pp.Status(lmstudioProxyProfile.Name); ready && port > 0 {
		go b.reconcileLMStudioProxyPortOnReadyForGeneration(lmstudioGeneration, port)
	}
	return pp, nil
}

// forwardProxyProcessNotification routes one proxy process's notifications to
// the engine each belongs to.
//
// A facade-scoped notification is addressed, so it goes to that engine's
// handler with that engine's generation. A process-scoped one is unaddressed
// and must be handled exactly once, not once per engine — routing a workload
// record through both handlers would record every job twice.
func (b *Broker) forwardProxyProcessNotification(
	ollamaGeneration, lmstudioGeneration uint64, method string, params json.RawMessage,
) {
	engine, bare := engines.SplitAddressedMethod(method)
	switch engine {
	case ollamaProxyProfile.Name:
		b.forwardProxyNotificationForGeneration(ollamaGeneration, method, params)
	case lmstudioProxyProfile.Name:
		b.forwardLMStudioProxyNotificationForGeneration(lmstudioGeneration, method, params)
	case "":
		if !b.routeProcessScopedProxyNotification(bare, params) {
			slog.Debug("ignoring unaddressed proxy notification", "method", bare)
		}
	default:
		slog.Warn("proxy addressed a notification to an unknown engine",
			"engine", engine, "method", bare)
	}
}

func (b *Broker) spawnWorkloadManager() (supervisedHandle, error) {
	wm, err := startWorkloadManager(b.workloadMgrPath, applog.LevelString(), b.relayDir, b.forwardWorkloadManagerNotification, b.clusterDirArgs()...)
	if err != nil {
		return nil, err
	}
	b.setWorkloadMgr(wm)
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceWorkload, Port: workloadHTTPPort})
	// A (re)started manager's anti-entropy set starts empty. Replay this node's
	// active local-origin workloads so a supervised restart mid-job doesn't
	// leave a later-joining peer unable to learn those jobs (and the heartbeat
	// non-empty). No-op on the initial spawn (the store is empty).
	b.rehydrateWorkloadManager(wm)
	slog.Info("workload-manager started", "path", b.workloadMgrPath, "pid", wm.cmd.Process.Pid)
	return wm, nil
}

// workloadReplayFrame is a lifecycle notification replayed to a (re)started
// workload-manager to rehydrate its re-sync set.
type workloadReplayFrame struct {
	method string
	params json.RawMessage
}

// workloadTerminalReplayWindowMs bounds how recently a terminal workload must
// have finished to be replayed to a (re)started workload-manager. It must be
// >= the manager's terminalRetention (2 × resyncInterval = 60s) so a restart
// mid-window doesn't drop the promised "re-assert a terminal a couple of times"
// guarantee; the manager re-arms its own expiry on replay, so an exact match is
// enough. Duplicated (not imported) because it's a private manager constant.
const workloadTerminalReplayWindowMs int64 = 60_000

// stateReplayMethod maps a stored workload state to the lifecycle method a
// replay frame should carry, so the manager re-tracks it with the right
// terminal/active disposition. Empty for an unknown state (skip).
func stateReplayMethod(state string) string {
	switch state {
	case "queued":
		return "workload:submitted"
	case "running":
		return "workload:started"
	case "completed":
		return "workload:completed"
	// "cancelled" rides workload:errored, the same frame the proxy emits it on:
	// the manager does not validate method against state, and consumers read
	// the state out of the payload. Every state the store can hold has to
	// appear here, because an omitted one is silently left out of the replay
	// and drops off the manager's re-sync set across a supervised restart.
	case "failed", "cancelled":
		return "workload:errored"
	default:
		return ""
	}
}

// activeLocalReplayFrames builds the workload:* frames for this node's active
// AND recently-terminal local-origin workloads. Only local-origin records are
// replayed — the origin is the single writer, so this node must never re-assert
// a peer's workloads. Recent terminals are included so the manager's two-
// heartbeat terminal re-sync window survives a supervised restart.
func (b *Broker) activeLocalReplayFrames() []workloadReplayFrame {
	var frames []workloadReplayFrame
	for _, r := range b.workloads.ReplayForNode(b.nodeID, workloadTerminalReplayWindowMs) {
		method := stateReplayMethod(r.State)
		if method == "" {
			continue
		}
		params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": r.Info})
		if err != nil {
			continue
		}
		frames = append(frames, workloadReplayFrame{method: method, params: params})
	}
	return frames
}

// rehydrateWorkloadManager replays this node's active local-origin workloads to
// wm's stdin so its re-sync set survives a supervised restart. Frames buffer in
// the pipe until the manager's read loop consumes them, so this works without
// waiting on the manager's readiness handshake.
func (b *Broker) rehydrateWorkloadManager(wm *workloadManagerProcess) {
	frames := b.activeLocalReplayFrames()
	for _, f := range frames {
		if err := wm.Forward(f.method, f.params); err != nil {
			slog.Warn("failed to replay active workload to workload-manager", "err", err)
			return
		}
	}
	if len(frames) > 0 {
		slog.Info("replayed active local workloads to workload-manager", "count", len(frames))
	}
}

func (b *Broker) spawnErrors() (supervisedHandle, error) {
	// Pass the resolved node UUID so nvpair-errors attributes this node's errors
	// to the same identity the broker stamps (and the cluster keys on), not its
	// hostname — keeping local-origin attribution and clear-by-id in lockstep.
	extra := append(b.clusterDirArgs(), "--node-id", b.nodeID)
	ep, err := startErrors(b.errorsPath, applog.LevelString(), b.relayDir, b.onErrorsUpdate, extra...)
	if err != nil {
		return nil, err
	}
	b.setErrors(ep)
	b.replayOllamaHostAliasError()
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: errorsHTTPPort})
	slog.Info("nvpair-errors started", "path", b.errorsPath, "pid", ep.cmd.Process.Pid)
	return ep, nil
}

func (b *Broker) spawnEngineMgr() (supervisedHandle, error) {
	// Tell engine-manager to serve its LAN HTTP surface on the fixed em port so
	// peers can fetch this node's model list at /v1/models, and its cluster-scoped
	// mTLS remote-control surface (ec) on the fixed control port. --cluster-dir
	// gates the ec surface: it is bound whenever a cluster dir is configured, but
	// admits a caller only while this node is a live cluster member (its TLS
	// identity is resolved per handshake from the live cluster dir), so a join or
	// leave needs no restart here.
	args := append(b.logLevelArgs(),
		"--http-port", fmt.Sprintf("%d", engineManagerHTTPPort),
		"--control-port", fmt.Sprintf("%d", engineControlPort))
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		args = append(args, "--reserved-port", fmt.Sprintf("%d", aliasPort))
	}
	args = append(args, b.clusterDirArgs()...)
	w, err := startRPCWorker("engine-manager", b.engineMgrPath, args, b.forwardEngineNotification)
	if err != nil {
		return nil, err
	}
	b.setEngineMgr(w)
	// Re-push the alias reservation before anything can drive this worker: a
	// respawned engine-manager starts with an empty reservation and must not
	// adopt or start a backend on the port the proxy alias owns.
	b.syncCurrentEngineOllamaHostAliasReservation()
	b.reconcileLMStudioProxyAfterEngineManagerReady()
	// Initial restore waits for both managed compatibility ports below. A later
	// engine-manager respawn sees the already-open gates and restores here.
	if b.managedPortOwnershipReady() {
		b.restoreEnabledEngines(w)
	}
	// Register engine-manager's HTTP surface so the daemon advertises em= on this
	// node's _nvpair-node record; peers enrich models from it. Fixed port (like
	// node-info's ni=), replayed across scanner restarts by regCache.
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceEngineManager, Port: engineManagerHTTPPort})
	// Advertise the ec remote-control surface too, whenever a cluster dir is
	// configured — which is exactly when engine-manager binds it. The port is
	// bound for the life of the process and admits callers by live membership
	// (an unclustered node presents no leaf, so every handshake is refused), so
	// this advertises a port that is genuinely listening in both states. That is
	// also why no re-registration is needed on a membership change: nothing about
	// the advertised address depends on whether this node is currently a member.
	if b.clusterDir != "" {
		b.registerService(noderec.RegisterParams{Service: noderec.ServiceEngineControl, Port: engineControlPort})
	}
	slog.Info("engine-manager started", "path", b.engineMgrPath, "pid", w.cmd.Process.Pid, "httpPort", engineManagerHTTPPort, "controlPort", engineControlPort)
	return w, nil
}

func (b *Broker) spawnManualNodes() (supervisedHandle, error) {
	w, err := startRPCWorker("manual-nodes", b.manualNodesPath, append(b.logLevelArgs(), b.clusterDirArgs()...), b.forwardManualNodesNotification)
	if err != nil {
		return nil, err
	}
	b.setManualNodes(w)
	slog.Info("manual-nodes started", "path", b.manualNodesPath, "pid", w.cmd.Process.Pid)
	return w, nil
}

func (b *Broker) spawnSettings() (supervisedHandle, error) {
	w, err := startRPCWorker("settings", b.settingsPath, b.logLevelArgs(), b.forwardSettingsNotification)
	if err != nil {
		return nil, err
	}
	b.setSettings(w)
	slog.Info("settings started", "path", b.settingsPath, "pid", w.cmd.Process.Pid)
	return w, nil
}

func (b *Broker) spawnJobScheduler() (supervisedHandle, error) {
	w, err := startRPCWorker("scheduler", b.schedulerPath, b.logLevelArgs(), b.forwardSchedulerNotification)
	if err != nil {
		return nil, err
	}

	// workloadEmitMu prevents an accepted workload transition from interleaving
	// with its baseline. schedulerFeedMu also holds discovery fanout until the
	// three ordered baselines are queued to the new child.
	b.workloadEmitMu.Lock()
	b.schedulerFeedMu.Lock()
	b.setScheduler(w)
	replayed := b.replayActiveWorkloadsToScheduler(w)
	telemetryReplayed := b.replayTelemetryToScheduler(w)
	if err := w.Notify("discovery:nodes-changed", b.store.Snapshot()); err != nil {
		slog.Warn("push discovery baseline to scheduler failed", "err", err)
	}
	b.schedulerFeedMu.Unlock()
	b.workloadEmitMu.Unlock()

	if replayed > 0 {
		slog.Info("replayed active workloads to scheduler", "count", replayed)
	}
	if telemetryReplayed > 0 {
		slog.Info("replayed telemetry to scheduler", "count", telemetryReplayed)
	}
	slog.Info("scheduler started", "path", b.schedulerPath, "pid", w.cmd.Process.Pid)
	return w, nil
}

// replayActiveWorkloadsToScheduler seeds a new scheduler through its existing
// workloads:upsert input. Callers hold workloadEmitMu and schedulerFeedMu so the
// replay is an ordered prefix of subsequent live fanout.
func (b *Broker) replayActiveWorkloadsToScheduler(w *rpcWorker) int {
	replayed := 0
	for _, info := range b.workloads.ActiveSnapshot() {
		params := map[string]json.RawMessage{"workloadInfo": info}
		if err := w.Notify("workloads:upsert", params); err != nil {
			slog.Warn("replay active workload to scheduler failed", "err", err)
			break
		}
		replayed++
	}
	return replayed
}

func (b *Broker) spawnClusterManager() (supervisedHandle, error) {
	// The manager owns <base>/cluster; the broker's clusterDir IS that subtree, so
	// its parent is the base — the same derivation resolveLocalNodeID performs.
	// Passing it keeps the only writer of the cluster dir and the workers reading it
	// on one directory.
	cm, err := startClusterManager(b.clusterMgrPath, applog.LevelString(), b.clusterManagerConfigDir(), b.relayDir, b.forwardClusterManagerNotification)
	if err != nil {
		return nil, err
	}
	b.setClusterMgr(cm)
	b.registerService(noderec.RegisterParams{Service: noderec.ServiceCluster, Port: clusterManagerHTTPPort})
	// Reflect the persisted clusterId (owned by nvpair-node-settings) back into the
	// cluster-manager, which doesn't persist it itself. Synchronous so the first
	// spawn restores before app:ready/readLoop (no client-visible race); also
	// covers a crash respawn.
	b.restoreClusterIdentity(cm)
	// Converge the scanner's advertised uuid= on the cluster principal. The
	// scanner spawns first and mints its own node-id before cluster-manager
	// writes identity.json, so on a fresh host its uuid= can start out diverged.
	go b.convergeScannerIdentity()
	slog.Info("cluster-manager started", "path", b.clusterMgrPath, "pid", cm.cmd.Process.Pid)
	return cm, nil
}

// convergeScannerIdentity waits for cluster-manager to report its node id (which
// confirms identity.json is written), then tells the scanner to re-resolve its
// identity so its advertised uuid= matches the cluster principal. This closes
// the fresh-host startup-order window (scanner minted node-id before
// cluster-manager wrote identity.json); a membership change reconverges via
// applyClusterIdentityChange and the scanner's own membership watch.
// Best-effort and idempotent.
func (b *Broker) convergeScannerIdentity() {
	cm := b.getClusterMgr()
	if cm == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := cm.Call(ctx, "cluster:get-node-id", nil); err != nil {
		slog.Warn("scanner identity convergence skipped: cluster-manager node id unavailable", "err", err)
		return
	}
	if sc := b.getScanner(); sc != nil {
		sc.reloadIdentity()
	}
}

// restoreClusterIdentity re-injects the persisted cluster identity from
// nvpair-node-settings into a freshly (re)spawned cluster-manager. The
// cluster-manager persists its member roster (members.json) but NOT the
// clusterId — that lives in nvpair-node-settings and must be reflected back via
// cluster:set-identity. Without this the manager comes up unclustered after a
// restart while still reporting its saved members, so cluster:get-node-id says
// "not clustered" while nodes:get-initial lists members (and roster reconcile,
// which bails on an empty clusterId, silently stops). Runs synchronously during
// the first spawn (before app:ready / readLoop, so a client's cluster:get-node-id
// sees the restored id with no race) and on every crash respawn. No-op when
// unclustered or when settings is unavailable.
func (b *Broker) restoreClusterIdentity(cm *clusterManagerProcess) {
	settings := b.getSettings()
	if settings == nil || cm == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := getSettingValue(ctx, settings, "settings/get-cluster-id")
	if id == "" {
		return // unclustered: nothing to restore
	}
	name := getSettingValue(ctx, settings, "settings/get-cluster-friendly-name")
	params, _ := json.Marshal(map[string]string{"clusterId": id, "clusterFriendlyName": name})
	if _, rpcErr, err := cm.Call(ctx, "cluster:set-identity", params); err != nil || rpcErr != nil {
		slog.Warn("restore cluster identity failed", "err", err, "rpcErr", rpcErr)
		return
	}
	slog.Info("restored cluster identity into cluster-manager", "clusterId", id)
}

// getSettingValue calls a nvpair-node-settings getter that returns {"value": "..."}
// and returns the string, or "" on any error. Mirrors the settings reads in
// prepareManagedOllamaFacade.
func getSettingValue(ctx context.Context, settings *rpcWorker, method string) string {
	result, rpcErr, err := settings.Call(ctx, method, nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read setting", "method", method, "err", err, "rpcErr", rpcErr)
		return ""
	}
	var r struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		slog.Warn("failed to decode setting", "method", method, "err", err)
		return ""
	}
	return r.Value
}

// persistClusterIdentity mirrors a cluster:identity-changed report from the
// cluster-manager (create / adopt-on-join / leave) into nvpair-node-settings, which
// is the durable home of the clusterId that restoreClusterIdentity re-injects on
// the next startup. Closing this loop is what makes a cluster join survive a
// restart and, crucially, what makes leaving a cluster stick — otherwise the
// stale clusterId in settings would be restored and the node would rejoin the
// cluster it just left. No-op when settings is unavailable; runs in a goroutine
// so it never blocks the cluster-manager's reader.
func (b *Broker) persistClusterIdentity(clusterID, friendlyName string) {
	settings := b.getSettings()
	if settings == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		setSettingValue(ctx, settings, "settings/set-cluster-id", clusterID)
		setSettingValue(ctx, settings, "settings/set-cluster-friendly-name", friendlyName)
	}()
}

// setSettingValue calls a nvpair-node-settings setter that takes {"value": "..."},
// logging (but not surfacing) any failure — the authoritative state already
// lives in the cluster-manager; settings is the durable mirror.
func setSettingValue(ctx context.Context, settings *rpcWorker, method, value string) {
	params, _ := json.Marshal(map[string]string{"value": value})
	if _, rpcErr, err := settings.Call(ctx, method, params); err != nil || rpcErr != nil {
		slog.Warn("failed to persist setting", "method", method, "err", err, "rpcErr", rpcErr)
	}
}

// logLevelArgs is the standard --log-level argument vector at the broker's
// currently-resolved level, recomputed on each (re)spawn so a runtime
// log/set-level is reflected when a worker restarts.
func (b *Broker) logLevelArgs() []string {
	return []string{"--log-level", applog.LevelString()}
}

// clusterManagerConfigDir is the base directory the cluster-manager should own,
// derived from the broker's cluster dir (<base>/cluster). Empty when the broker
// resolved no cluster dir, which leaves the manager on its own default — the one
// case where the two can still diverge, and the reason an unresolved cluster dir
// is reported loudly at startup.
func (b *Broker) clusterManagerConfigDir() string {
	if b.clusterDir == "" {
		return ""
	}
	return filepath.Dir(b.clusterDir)
}

// clusterDirArgs returns ["--cluster-dir", dir] when a cluster dir is
// configured, else nil. Threaded into every worker that does cluster-scoped
// inter-node mTLS (nvpair-errors, nvpair-workload-manager, nvpair-node-scanner,
// nvpair-manual-nodes, nvpair-engine-manager) so each serves/dials pinned cluster
// peers once this node is clustered; absent it, those workers stay on plain HTTP
// (or, for engine-manager's ec surface, don't bind it at all). nvpair-node-info is
// deliberately excluded — it stays plain HTTP even on a clustered node (see the
// spawnNodeInfo call site), so it is never passed --cluster-dir.
func (b *Broker) clusterDirArgs() []string {
	if b.clusterDir == "" {
		return nil
	}
	return []string{"--cluster-dir", b.clusterDir}
}

// resolveLocalNodeID resolves this node's stable per-host UUID from the same
// on-disk identity the node-scanner and cluster-manager use (nodeid.Resolve
// prefers cluster/identity.json, then node-id.json, else mints and persists a
// fresh one), so the broker's originatedFrom/errors stamp matches this node's
// advertised hostUuid. The base is the parent of the cluster dir (which is
// <base>/cluster); an empty cluster dir selects nodeid's shared default. There
// is no hostname fallback: nodeid.Resolve returns empty only if the OS CSPRNG
// is unavailable, and a node with no stable identity must not run (it would
// stamp inconsistently across the cluster), so we fail loudly instead.
func resolveLocalNodeID(clusterDir string) string {
	base := ""
	if clusterDir != "" {
		base = filepath.Dir(clusterDir)
	}
	id := nodeid.Resolve(base)
	if id == "" {
		log.Fatal("could not resolve a stable node UUID (system CSPRNG unavailable); refusing to start without a node identity")
	}
	return id
}

// applyClusterIdentityChange converges this node's advertisement after the
// cluster-manager reports cluster:identity-changed (this node created or joined a
// cluster, or left one).
//
// It deliberately does NOT restart the cluster-scoped workers. Each of them holds
// a live view of the cluster dir (nvpair-shared/clustertrust) and re-derives its
// own membership, so it changes personality in place: the proxies' LAN mTLS
// ingress, nvpair-errors' and nvpair-workload-manager's inter-node interfaces,
// engine-manager's remote-control surface, and manual-node probes all follow a
// join or leave without a new process. Restarting them instead was both
// disruptive (it recycled inference ingress and discovery on every membership
// change) and unreliable: it was a single unverified attempt whose outcome
// depended on the workers reading a cluster dir the manager was writing at that
// instant, and a worker that read it a moment too early stayed unclustered for
// the rest of its life — in the roster and ranked by the scheduler, yet
// exchanging no cluster traffic.
//
// Two workers are told directly, because they publish this node's membership
// rather than deriving it. The scanner owns the mDNS record: cluster-uuid= is how
// a peer finds the pin for this node, so it should flip now rather than at the
// scanner's next membership poll. node-info carries the same fact over plain
// HTTP, which is a peer's only way to learn it when this node's mDNS record does
// not reach it — and it holds no cluster dir, so the value has to be pushed. Both
// are idempotent and run on their own goroutine, because they are
// bounded-but-blocking writes and the caller is the cluster-manager's reader.
func (b *Broker) applyClusterIdentityChange() {
	if b.clusterDir == "" {
		return
	}
	slog.Info("cluster identity changed; converging this node's advertised cluster identity")
	if sc := b.getScanner(); sc != nil {
		go sc.reloadIdentity()
	}
	go b.pushClusterIdentityToNodeInfo()
}

// clusterPrincipal is the cluster principal this node currently holds, or empty
// when it belongs to no cluster. It is read from the trust store on each call
// rather than cached: membership changes under the broker while it runs, and this
// is the value peers key their pins on.
func (b *Broker) clusterPrincipal() string {
	if b.clusterDir == "" {
		return ""
	}
	mesh := clustertrust.Open(b.clusterDir)
	mesh.Refresh()
	if !mesh.Clustered() {
		return ""
	}
	return mesh.NodeUUID()
}

// pushClusterIdentityToNodeInfo sends node-info the current cluster principal.
// Best-effort on a node-info that isn't running (never spawned, or mid-restart):
// the next spawn pushes again. A failed write is warned about rather than traced,
// because until the next push lands peers cannot learn this node's membership
// over HTTP — which is the whole point of reporting it.
func (b *Broker) pushClusterIdentityToNodeInfo() {
	np := b.getNodeInfo()
	if np == nil {
		return
	}
	if err := np.SetClusterIdentity(b.clusterPrincipal()); err != nil {
		slog.Warn("failed to push cluster identity to node-info", "err", err)
	}
}

// forwardEngineNotification / forwardManualNodesNotification /
// forwardSettingsNotification are the per-worker notification hooks. For
// now they only demux errors:report / errors:clear into the nvpair-errors
// pipeline; the rest of each worker's notification stream is logged and
// dropped until its control-plane relay is wired in.
func (b *Broker) forwardEngineNotification(method string, params json.RawMessage) {
	if method == "engine:settings-request" {
		b.handleSettingsRelay(params)
		return
	}
	if method == "engine:settings-cancel" {
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(params, &p) == nil {
			b.settingsRelayMu.Lock()
			cancel := b.settingsCancels[p.ID]
			b.settingsRelayMu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		return
	}
	if b.dispatchErrorsNotif("engine-manager", method, params) {
		return
	}
	if method == "engine:ready" {
		b.reconcileLMStudioProxyAfterEngineManagerReady()
		go func() {
			b.engineConfigMu.Lock()
			defer b.engineConfigMu.Unlock()
			if b.engineSettingsLoaded && b.engineSettingsError == nil && len(b.engineSettings) > 0 {
				b.publishSettingsLocked()
			}
		}()
	}
	if method == noderec.MethodSubscribe {
		// engine-manager subscribes upward for its ec peer set (nodes exposing
		// the remote-control surface): wire it to the relay directory and push
		// discovery:nodes snapshots down its stdin, exactly like nvpair-errors.
		b.handleEngineSubscribe(params)
		return
	}
	// engine:ready / engine:state-changed / engine:install-progress /
	// engine:pull-progress are re-emitted verbatim to engine:subscribe'd clients
	// (their method names are already engine:-prefixed, so no translation). Off
	// by default.
	b.engineMu.Lock()
	subscribed := b.engineSubscribed
	b.engineMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify(method, params); err != nil {
		slog.Warn("forward engine notification failed", "method", method, "err", err)
	}
}

// handleEngineSubscribe wires engine-manager's discovery:subscribe into the
// relay directory: it registers a subscriber whose Send pushes a
// discovery:nodes snapshot down engine-manager's stdin, then delivers the
// initial snapshot. A re-subscribe drops the prior registration first so it
// isn't double-fed; worker exit clears it (see the engine-manager onExit hook).
func (b *Broker) handleEngineSubscribe(params json.RawMessage) {
	if b.relayDir == nil {
		return
	}
	em := b.getEngineMgr()
	if em == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		if err := em.Notify(noderec.NotifyNodes, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to engine-manager", "err", err)
		}
	}
	b.engineRelayMu.Lock()
	if b.engineRelaySubID != 0 {
		b.relayDir.Unsubscribe(b.engineRelaySubID)
	}
	id, sub, err := subscribeRelay(b.relayDir, params, send)
	b.engineRelaySubID = id
	b.engineRelayMu.Unlock()
	if err != nil {
		slog.Warn("engine-manager sent invalid discovery:subscribe", "err", err)
		return
	}
	b.relayDir.Deliver(sub)
}

// clearEngineRelaySub drops engine-manager's relay subscription. Called from its
// supervisor onExit so a crashed/stopped worker doesn't leave a dangling
// subscriber pushing to a closed stdin.
func (b *Broker) clearEngineRelaySub() {
	b.engineRelayMu.Lock()
	if b.engineRelaySubID != 0 && b.relayDir != nil {
		b.relayDir.Unsubscribe(b.engineRelaySubID)
		b.engineRelaySubID = 0
	}
	b.engineRelayMu.Unlock()
}

func (b *Broker) forwardManualNodesNotification(method string, params json.RawMessage) {
	if b.dispatchErrorsNotif("manual-nodes", method, params) {
		return
	}
	// Merge manual nodes into the same discovery store the scanner feeds,
	// so manual + mDNS nodes surface on one discovery:get-nodes /
	// discovery:nodes-changed snapshot.
	switch method {
	case "node/discovered", "node/updated":
		var s manualNodeStatus
		if err := json.Unmarshal(params, &s); err != nil {
			slog.Warn("manual-nodes emitted invalid node payload", "method", method, "err", err)
			return
		}
		if s.ID == "" {
			slog.Warn("manual-nodes node payload missing id", "method", method)
			return
		}
		b.upsertManualNode(s)
		slog.Debug("manual node event", "event", method, "id", s.ID)
	case "node/removed":
		var s manualNodeStatus
		if err := json.Unmarshal(params, &s); err != nil {
			slog.Warn("manual-nodes emitted invalid removed payload", "err", err)
			return
		}
		b.removeManualNode(s.ID)
		slog.Debug("manual node event", "event", method, "id", s.ID)
	case "ready":
		slog.Info("manual-nodes reported ready", "params", string(params))
	default:
		slog.Debug("ignoring manual-nodes notification", "method", method)
	}
}

// trackManualNode records the discovery-store key a manual node currently
// occupies and reports the previous key when it changed (a manual node's
// node-info first reported its real hostUuid, moving it off the manual id). On a
// rekey it drops the manual claim under the old key (the scanner's claim, if
// any, survives); the caller rebridges the proxy overlay off the old key.
// upsertManualNode ingests a manual node status: it records the alias's payload,
// keys the discovery store by the node's operational identity (its hostUuid once
// node-info reports one, else the manual id), and bridges the proxy candidate
// under that same key so scheduler priority and scheduledOn resolve to it. If
// the alias's key changed (node-info revealed its real UUID) the
// old key is reprojected from a surviving alias or released.
func (b *Broker) upsertManualNode(s manualNodeStatus) {
	en := manualToEnriched(s)
	key := en.storeKey()
	receivedAt := time.Now()

	b.manualMu.Lock()
	oldKey, existed := b.manualNodeKeys[s.ID]
	b.manualNodeKeys[s.ID] = key
	b.manualNodeStatuses[s.ID] = manualNodeStatusEntry{status: s, receivedAt: receivedAt}
	b.manualMu.Unlock()

	b.store.Upsert(en, sourceManual)
	b.ingestTelemetryAt(sourceManual, manualNodeTelemetry(s, key), receivedAt)
	// Bridge a reachable manual node into each engine's proxy (ollama-proxy /
	// lmstudio-proxy) so inference can route to it; an unreachable engine is
	// pulled back out. No-op for a proxy the broker doesn't supervise.
	b.bridgeManualNode(s, key)

	if existed && oldKey != key {
		// This alias moved off oldKey: refresh oldKey from a surviving alias, or
		// release its store/proxy claim if this was its last owner.
		b.reprojectOrRelease(oldKey)
	}
}

// removeManualNode forgets a manual alias, then reprojects or releases the key
// it occupied. When another alias for the same machine still owns the key, the
// node stays and is reprojected from that survivor's last-seen payload — so the
// removed alias's address/models don't linger (the survivor's steady-state probe
// emits no update to restore itself); only the final alias's removal drops the
// store/proxy claim.
func (b *Broker) removeManualNode(id string) {
	b.manualMu.Lock()
	key, ok := b.manualNodeKeys[id]
	delete(b.manualNodeKeys, id)
	delete(b.manualNodeStatuses, id)
	b.manualMu.Unlock()
	if !ok {
		return
	}
	b.reprojectOrRelease(key)
}

// reprojectOrRelease refreshes or drops a key's manual claim after an alias
// stopped pointing at it. If another manual alias still resolves to key, that
// survivor's payload is re-projected into the store's single manual slot and its
// proxy candidate rebridged; otherwise the store and proxy manual claim is
// released.
func (b *Broker) reprojectOrRelease(key string) {
	if key == "" {
		return
	}
	b.manualMu.Lock()
	survivor, ok := b.survivingAliasLocked(key)
	b.manualMu.Unlock()
	if ok {
		b.store.Upsert(manualToEnriched(survivor.status), sourceManual)
		b.bridgeManualNode(survivor.status, key)
		b.ingestTelemetryAt(sourceManual, manualNodeTelemetry(survivor.status, key), survivor.receivedAt)
		return
	}
	b.store.Remove(key, sourceManual)
	b.removeManualNodeFromProxies(key)
	b.removeTelemetry(sourceManual, key)
}

// survivingAliasLocked returns the last-seen status of a manual alias still
// resolving to key, picking the lexicographically smallest alias id for a stable
// choice. Callers must hold manualMu.
func (b *Broker) survivingAliasLocked(key string) (manualNodeStatusEntry, bool) {
	best := ""
	for id, k := range b.manualNodeKeys {
		if k == key && (best == "" || id < best) {
			best = id
		}
	}
	if best == "" {
		return manualNodeStatusEntry{}, false
	}
	return b.manualNodeStatuses[best], true
}

// clearManualNodesState is the manual-nodes supervisor's clearHandle: on a
// crash it drops the worker handle and evicts every manual-origin node from
// the discovery store. The restarted process comes up with no entries (it
// keeps no persistent state and the broker doesn't re-feed them), so
// leaving the old manual nodes in the snapshot would strand stale entries
// that never age out. Clients re-add manual nodes after a restart.
func (b *Broker) clearManualNodesState() {
	b.setManualNodes(nil)
	b.manualMu.Lock()
	keys := b.manualNodeKeys
	b.manualNodeKeys = make(map[string]string)
	b.manualNodeStatuses = make(map[string]manualNodeStatusEntry)
	b.manualMu.Unlock()
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		// Aliases can share a key; drop each unique claim once.
		if seen[key] {
			continue
		}
		seen[key] = true
		// Drop only the manual claim; a co-located scanner node keeps its record.
		b.store.Remove(key, sourceManual)
		b.removeTelemetry(sourceManual, key)
		// Pull the now-orphaned node out of every proxy too, so inference
		// doesn't keep a stale manual target the crashed prober can no
		// longer vouch for. Clients re-add manual nodes after the restart.
		b.removeManualNodeFromProxies(key)
	}
}

func (b *Broker) forwardSettingsNotification(method string, params json.RawMessage) {
	if b.dispatchErrorsNotif("settings", method, params) {
		return
	}
	// connection/cluster-identity and connection/cluster-auto-sync are the
	// change-only cluster signals nvpair-node-settings pushes. Re-emit them
	// verbatim and unconditionally — the same way the UI receives them from
	// the settings subprocess today (no opt-in subscribe for these).
	switch method {
	case "connection/cluster-identity", "connection/cluster-auto-sync":
		if err := b.codec.Notify(method, params); err != nil {
			slog.Warn("forward settings notification failed", "method", method, "err", err)
		}
	default:
		slog.Debug("ignoring settings notification", "method", method)
	}
}

// forwardSchedulerNotification is the hook startRPCWorker invokes on the
// scheduler's reader goroutine for every notification it emits. Its errors:*
// reports go into the pipeline like every other producer's; schedule:priority
// is cached and fanned out to the matching proxy via node/set-priority.
func (b *Broker) forwardSchedulerNotification(method string, params json.RawMessage) {
	if b.dispatchErrorsNotif("scheduler", method, params) {
		return
	}
	if method == "schedule:priority" {
		var p schedulerwire.EnginePriority
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("bad schedule:priority payload", "err", err)
			return
		}
		generation, fresh := b.cachePrioritySnapshot(p.Snapshot())
		if !fresh {
			// The scheduler's duplicate emission of one recompute. Dropped here,
			// before it mints a generation, so nothing downstream sees it at all.
			return
		}
		// Dispatch off the reader goroutine: the node/set-priority round-trip
		// blocks on the proxy's response, so calling it inline would stall the
		// scheduler's notification stream.
		go b.applyPriority(generation)
		return
	}
	slog.Debug("ignoring scheduler notification", "method", method)
}

// forwardNodeInfoNotification is the hook startNodeInfo invokes on node-info's
// reader goroutine for every notification it emits. Its observed local addresses
// go to the scanner, which owns what this node advertises: node-info learns which
// addresses peers actually reach us on, and that is the only direct evidence
// address selection can have about reachability from somewhere else.
func (b *Broker) forwardNodeInfoNotification(method string, params json.RawMessage) {
	if method != noderec.NotifyObservedAddresses {
		slog.Debug("ignoring node-info notification", "method", method)
		return
	}
	var p noderec.ObservedAddressesParams
	if err := json.Unmarshal(params, &p); err != nil {
		slog.Warn("bad observed-addresses payload", "err", err)
		return
	}
	sp := b.getScanner()
	if sp == nil {
		return
	}
	// Off the reader goroutine: the relay waits on the scanner's response, and
	// blocking here would stall node-info's stdout.
	go sp.pushObservedAddresses(p.Addresses)
}

// cachePrioritySnapshot records a new ranking and returns its generation, or
// reports fresh=false when the ranking is byte-identical to the cached one.
//
// The cache is process-wide, not per engine, and that is what makes the dedupe
// work: one scheduler recompute emits the same ranking once per engine
// (schedule.go computes one order and loops the engine list), so the second
// frame is a duplicate that differs only in a routing key the proxy never sees.
// Comparing per engine would never match those two against each other.
//
// Dropping the duplicate here, before it mints a generation, is deliberate.
// The alternative — comparing content at delivery — silently breaks
// repushPriority, whose whole job is to re-send an unchanged ranking to a proxy
// that lost its state, and which is therefore always byte-identical to the last
// delivery.
func (b *Broker) cachePrioritySnapshot(priority schedulerwire.Priority) (generation uint64, fresh bool) {
	b.schedMu.Lock()
	defer b.schedMu.Unlock()

	if b.havePriority && b.lastPriority.SameRanking(priority) {
		return b.priorityGeneration, false
	}
	b.lastPriority = priority.Clone()
	b.havePriority = true
	b.priorityGeneration++
	b.lastPriority.Generation = b.priorityGeneration
	return b.priorityGeneration, true
}

// applyPriority delivers one cached generation to every live proxy. A stale
// generation is silently skipped; an engine with no proxy is a logged no-op and
// picks the snapshot up from the replay on its next spawn.
//
// It fans out rather than targeting one engine because the ranking is node-wide
// and the cache is now process-wide: with the duplicate emission dropped at
// ingestion, the surviving frame is whichever engine the scheduler emitted
// first, so delivering only to that engine's proxy would starve the other.
func (b *Broker) applyPriority(generation uint64) {
	b.deliverPrioritySnapshot(generation, func(priority schedulerwire.Priority) {
		b.sendPriority(context.Background(), priority)
	})
}

// sendPriority delivers one ranking to every live proxy. Shared by the
// scheduler's gated fan-out and the ungated spawn replay, so both agree on how
// a delivery is made and only differ on when one is allowed.
func (b *Broker) sendPriority(ctx context.Context, priority schedulerwire.Priority) {
	params, err := json.Marshal(priority)
	if err != nil {
		slog.Warn("marshal node/set-priority failed", "err", err)
		return
	}
	for _, target := range b.livePriorityTargets() {
		if _, rpcErr, err := target.proxy.Call(ctx, "node/set-priority", params); err != nil {
			slog.Warn("node/set-priority call failed", "engine", target.engine, "err", err)
		} else if rpcErr != nil {
			slog.Warn("node/set-priority rejected", "engine", target.engine,
				"code", rpcErr.Code, "msg", rpcErr.Message)
		}
	}
}

// priorityTarget is one proxy to deliver a ranking to, with the engine name
// kept for log context.
type priorityTarget struct {
	engine string
	proxy  *proxyProcess
}

// livePriorityTargets returns the distinct live proxies to deliver a ranking to.
//
// Distinct by handle, not by engine: once one process hosts every facade, two
// engine entries resolve to the same *proxyProcess, and calling it once per
// engine would deliver the same generation twice. The child treats the repeat as
// an acknowledged no-op, but sending it is still pointless work on the request
// path's critical peer.
func (b *Broker) livePriorityTargets() []priorityTarget {
	var out []priorityTarget
	seen := make(map[*proxyProcess]bool)
	for _, profile := range engineProxyProfiles {
		p := b.engineProxyHandle(profile)
		if p == nil {
			slog.Debug("no proxy for engine; ranking arrives on its next spawn", "engine", profile.Name)
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, priorityTarget{engine: profile.Name, proxy: p})
	}
	return out
}

// deliverPrioritySnapshot serializes deliveries and invokes deliver only if
// generation is still current. It is split from the proxy call so ordering can
// be verified without a subprocess.
func (b *Broker) deliverPrioritySnapshot(generation uint64, deliver func(schedulerwire.Priority)) {
	b.priorityApplyMu.Lock()
	defer b.priorityApplyMu.Unlock()

	b.schedMu.Lock()
	if b.priorityGeneration != generation {
		b.schedMu.Unlock()
		return
	}
	priority := b.lastPriority.Clone()
	b.schedMu.Unlock()

	deliver(priority)
}

// replayProxyStateAfterSpawn restores the broker-pushed state a fresh proxy
// process does not have. It is wired to every proxy supervisor's onSpawned,
// which is the only event that loses that state — a set-port rebind keeps the
// child's in-process state, so it needs no replay.
//
// The spawn hook rather than the proxy's own "ready" notification, for a
// specific reason: spawn publishes the handle before returning, so a replay
// from here is guaranteed to find it. A replay triggered by "ready" races that
// publication, because the child can emit ready before the broker has stored
// the handle — and a replay that misses the handle is dropped by the generation
// check and never retried, leaving the new child with no ranking at all.
func (b *Broker) replayProxyStateAfterSpawn() {
	// Abandoned once teardown starts, which the onSpawned field comment
	// requires of any callback that talks to the worker: this one calls the
	// proxy, and a teardown is about to close that worker's stdin. It runs on
	// its own goroutine so it cannot stall Stop, but there is no reason to
	// spend a call timeout re-seeding a child that is going away.
	ctx, cancel := b.proxyBringUpContext()
	defer cancel()
	b.repushPriority(ctx)
}

// repushPriority re-sends the cached ranking after a proxy (re)spawns, because
// a fresh child starts with no scheduler state. A no-op when nothing has been
// cached yet.
//
// It reuses the current generation rather than minting one, so the child's
// idempotency check makes a repush to a proxy that never lost its state
// harmless — which is what lets several spawn paths call this without
// coordinating.
func (b *Broker) repushPriority(ctx context.Context) {
	// Serialized against the scheduler's own deliveries, but deliberately not
	// gated on a generation.
	//
	// The gate stops an older generation from overwriting a newer delivery,
	// which is right for the scheduler's fan-out and wrong here. A replay races
	// a snapshot that arrived while no handle was published: that snapshot found
	// no targets, and gating the replay on the generation it read then discards
	// it as stale, so neither is delivered. The proxy is left with an empty
	// ranking, reserveCandidate returns early, and no reservations are taken at
	// all — the behaviour the shared map exists to provide — until the next
	// genuine ranking change.
	//
	// Unconditional is cheap: the child applies at most one snapshot per
	// generation, so a repush to a proxy that never lost its state costs one
	// ignored call.
	b.priorityApplyMu.Lock()
	defer b.priorityApplyMu.Unlock()

	b.schedMu.Lock()
	have := b.havePriority
	priority := b.lastPriority.Clone()
	b.schedMu.Unlock()
	if !have {
		return
	}
	b.sendPriority(ctx, priority)
}

// onErrorsUpdate relays an errors:update snapshot from nvpair-errors straight
// to the connected client. nvpair-errors pushes the full sorted ServiceError
// list on every change; following the established error-reporting protocol
// the broker forwards it verbatim and unconditionally (no opt-in
// subscribe), allowing the bundled UI or another client to fan errors:update
// into its presentation layer. Invoked on the nvpair-errors reader
// goroutine.
func (b *Broker) onErrorsUpdate(params json.RawMessage) {
	if err := b.codec.Notify(methodErrorsUpdate, params); err != nil {
		slog.Warn("relay errors:update failed", "err", err)
	}
}

// startOptionalWorker creates a supervisor for an optional (non-fatal)
// worker, wires its crash/recovery callbacks (report into the pipeline,
// drop the handle while down), and starts it. It returns the supervisor,
// or nil when the binary wasn't resolved or the first spawn failed — in
// both cases the broker simply runs without that worker. The caller defers
// Stop on a non-nil result.
func (b *Broker) startOptionalWorker(name, path string, spawn func() (supervisedHandle, error), clearHandle func()) *supervisor {
	if path == "" {
		slog.Info("worker path not resolved; running without it", "worker", name)
		return nil
	}
	sup := newSupervisor(name, defaultRestartPolicy(), spawn)
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks(name, clearHandle)
	if err := sup.Start(); err != nil {
		slog.Warn("worker failed to start; continuing without it", "worker", name, "path", path, "err", err)
		return nil
	}
	return sup
}

// Serve runs the JSON-RPC loop until the peer disconnects, the context is
// cancelled, or a "shutdown" request is received. The returned error is
// nil on graceful close; a non-nil error indicates an unrecoverable
// transport problem or a failure to spawn the scanner.
//
// Ordering matters: the scanner is spawned BEFORE "app:ready" is
// emitted so the client can assume, once it sees app:ready, that
// discovery events are already flowing into the broker's store (even
// if no nodes have been found yet — mDNS browsing takes a moment to
// populate).
// workloadHistoryFlushJoinTimeout bounds how long Serve waits for the history
// flusher's shutdown flush before giving up, so a hung disk can't wedge exit.
const workloadHistoryFlushJoinTimeout = 5 * time.Second

// runWorkloadHistoryFlusher starts the workload store's coalescing flusher and
// returns a stop func that cancels it and waits (bounded) for its final shutdown
// flush to complete. The returned stop func is the SOLE terminator, so callers
// must always invoke it (Serve defers it).
//
// The flusher runs on a context DETACHED from ctx (context.WithoutCancel): a
// shutdown signal (SIGINT/SIGTERM or JSON-RPC shutdown) cancels ctx before
// Serve's deferred worker teardown runs, and the proxies can still emit a
// terminal workload:errored while tearing down. If the flusher's context were a
// child of ctx, Store.Run would final-flush and exit on that signal — before
// those teardown terminals reached the store — and the later stop() would only
// join an already-finished goroutine, leaving them unpersisted. Detaching keeps
// the flusher alive until stop(), which Serve defers AFTER the worker-teardown
// defers (LIFO), so producers are quiescent before the final flush + join.
// b.workloads must already have persistence configured (WithPersistence).
func (b *Broker) runWorkloadHistoryFlusher(ctx context.Context) func() {
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.workloads.Run(fctx, 0, 0)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(workloadHistoryFlushJoinTimeout):
			slog.Warn("workload history flusher did not finish its shutdown flush in time")
		}
	}
}

func (b *Broker) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.setServeCtx(ctx)
	defer cancel()

	// Enable durable workload history: load the prior session's terminal
	// records so a late-subscribing client / workloads:get-initial has a
	// baseline, then start the coalescing flusher (which also flushes on
	// ctx cancellation). Best-effort — an unresolved data dir or a bad file
	// leaves the store purely in-memory; it never blocks startup. The returned
	// stop func is deferred so the flusher's shutdown flush is JOINED before
	// Serve returns — otherwise the process could exit after cancel() but before
	// the goroutine's final flush, losing a workload that finished since the
	// last periodic flush. Registered after the top-level defer cancel(), so it
	// runs first (LIFO): late terminal events from workers shutting down are
	// still captured, then the flusher is cancelled and joined.
	if p, err := appdir.Path("workloads-history.json"); err != nil {
		slog.Warn("could not resolve workload history path; running in-memory only", "err", err)
	} else if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		slog.Warn("could not create data dir for workload history; running in-memory only", "err", err)
	} else {
		b.workloads.WithPersistence(p)
		if err := b.workloads.Load(); err != nil {
			slog.Warn("failed to load persisted workload history", "path", p, "err", err)
		}
		stopFlusher := b.runWorkloadHistoryFlusher(ctx)
		defer stopFlusher()
	}

	// Bound how long a lost terminal event can keep a remote workload displaying
	// as in-flight. Independent of persistence above: it guards the live set.
	go b.runStaleWorkloadSweep(ctx)
	go b.refreshEngineSettings(ctx)

	// The scanner is the broker's core worker. Its supervisor's first
	// spawn is synchronous so a hard startup failure stays fatal — the
	// broker has no job without discovery. Once up, the supervisor watches
	// it and (when auto-restart is enabled) respawns it on an unexpected
	// exit instead of leaving the discovery snapshot frozen.
	b.scannerSup = newSupervisor("scanner", defaultRestartPolicy(), b.spawnScanner)
	b.scannerSup.onCrash, b.scannerSup.onRecovered = b.supervisedWorkerCallbacks("scanner", func() { b.setScanner(nil) })
	if err := b.scannerSup.Start(); err != nil {
		return fmt.Errorf("start scanner: %w", err)
	}
	defer b.scannerSup.Stop()

	// nvpair-errors is the service-error datastore and the backbone of the
	// error-surfacing pipeline: producers' errors:report / errors:clear
	// are forwarded into it and its errors:update snapshots are relayed to
	// the client. Spawn it early (right after the scanner, before the
	// other producers) so a sink exists by the time they start emitting.
	// It runs with --peer-sync for cross-node error propagation. Like the
	// other auxiliaries a failed first
	// spawn (or never-resolved binary) is non-fatal — the broker logs and
	// runs with the pipeline disabled (producer errors are dropped).
	if b.errorsPath != "" {
		b.errorsSup = newSupervisor("errors", defaultRestartPolicy(), b.spawnErrors)
		b.errorsSup.onCrash = b.errorsCrashFallback
		b.errorsSup.onRecovered = func() { slog.Info("nvpair-errors recovered") }
		if err := b.errorsSup.Start(); err != nil {
			slog.Warn("nvpair-errors failed to start; continuing with the error pipeline disabled", "path", b.errorsPath, "err", err)
			b.errorsSup = nil
		} else {
			defer b.errorsSup.Stop()
		}
	} else {
		slog.Info("nvpair-errors path not resolved; running with the error pipeline disabled")
	}

	// Settings and engine-manager must be available before either proxy starts.
	// Managed ownership prepares Ollama's deferred move and lets engine-manager
	// safely move an identified LM Studio command runtime. Unknown owners are
	// never terminated; preparation instead selects an explicit fallback.
	b.settingsSup = b.startOptionalWorker("settings", b.settingsPath, b.spawnSettings, func() { b.setSettings(nil) })
	if b.settingsSup != nil {
		defer b.settingsSup.Stop()
	}
	b.prepareOllamaHostAliasCandidate()
	b.engineMgrSup = b.startOptionalWorker("engine-manager", b.engineMgrPath, b.spawnEngineMgr, func() { b.setEngineMgr(nil); b.clearEngineRelaySub() })
	if b.engineMgrSup != nil {
		defer b.engineMgrSup.Stop()
	}
	b.migrateLegacyEngineSettings()
	b.prepareEnabledFacades()

	// node-info is an auxiliary worker: spawning it lets the broker's own
	// host advertise its hardware inventory on the network, but it is NOT
	// essential to the broker's core job (serving the
	// discovery snapshot). A spawn failure — or a never-resolved binary
	// (empty path) — is therefore non-fatal: we log and carry on rather
	// than tearing down discovery.
	if b.nodeInfoPath != "" {
		b.nodeInfoSup = newSupervisor("node-info", defaultRestartPolicy(), b.spawnNodeInfo)
		b.nodeInfoSup.onCrash, b.nodeInfoSup.onRecovered = b.supervisedWorkerCallbacks("node-info", func() { b.setNodeInfo(nil) })
		if err := b.nodeInfoSup.Start(); err != nil {
			slog.Warn("node-info failed to start; continuing without local node advertisement", "path", b.nodeInfoPath, "err", err)
			b.nodeInfoSup = nil
		} else {
			defer b.nodeInfoSup.Stop()
		}
	} else {
		slog.Info("node-info path not resolved; running without local node advertisement")
	}

	// nvpair-proxy is another auxiliary worker: one process that runs a local
	// reverse proxy per enabled engine and exposes a control plane the broker
	// relays to clients. Like node-info, a spawn failure or never-resolved
	// binary is non-fatal. It does NOT gate app:ready: each facade announces
	// its listen port asynchronously via a "ready" notification (after its HTTP
	// bind), so clients learn the port by polling <engine>-proxy:get-status
	// rather than by the presence of app:ready.
	//
	// One supervisor for every engine. That is what puts the estimated-work
	// reservations of all facades in one place, which is the point of the
	// unified proxy; the cost is shared fate, so a crash takes every engine's
	// facade down and the supervisor brings them all back together.
	anyProxyEnabled := false
	for _, profile := range engineProxyProfiles {
		if b.proxyEnabled(profile) {
			anyProxyEnabled = true
			continue
		}
		reason, report := proxyDisabledReason(b.proxyPath)
		slog.Info("engine proxy not enabled; running without one",
			"engine", profile.Name, "reason", reason)
		// Preparation was skipped for a disabled engine, so managedFacade is
		// necessarily false and there is no facade to block. Surface the
		// unavailability directly instead, when it is worth surfacing.
		if report {
			b.reportProxyUnavailable(profile, reason)
		}
		b.finishEngineProxyStartup(profile)
	}
	if anyProxyEnabled {
		b.proxySup = newSupervisor(engines.ProxyComponent, defaultRestartPolicy(), b.spawnProxy)
		b.configureProxySupervisorCallbacks(b.proxySup)
		if err := b.proxySup.Start(); err != nil {
			slog.Warn("proxy failed to start; continuing without local engine proxies",
				"path", b.proxyPath, "err", err)
			b.proxySup = nil
			// No facade will come up, so both engines take the terminal path:
			// report the blocked claim where one was made, then finish, which
			// is what returns the OLLAMA_HOST alias reservation.
			if b.ollamaState().managedFacade.Load() {
				b.blockManagedOllamaFacade("the proxy could not be started")
			}
			b.finishOllamaProxyTerminal()
			if b.lmstudioState().managedFacade.Load() {
				_, _ = b.blockManagedLMStudioFacade("the proxy could not be started", nil)
			}
			b.finishLMStudioProxyTerminal()
		} else {
			defer b.proxySup.Stop()
		}
	}

	// Restore engines and begin both advertising loops only after both proxy
	// startup attempts have established either readiness or a terminal outcome.
	// This prevents a restored engine from taking a persisted proxy port before
	// the broker can resolve ownership.
	go b.runEngineAvailabilityAfterPortGates(ctx, b.runAutoAdvertise, b.runAutoAdvertiseLMStudio)

	// nvpair-workload-manager is another auxiliary worker: it relays local
	// workload lifecycle events to peer nodes and surfaces peer events
	// back. Like node-info and the proxy, a spawn failure or never-resolved
	// binary is non-fatal — the broker simply runs without cluster workload
	// relay. It does NOT gate app:ready: it has no readiness handshake the
	// broker waits on, and workload traffic only flows once the proxy emits
	// events.
	if b.workloadMgrPath != "" {
		b.workloadMgrSup = newSupervisor("workload-manager", defaultRestartPolicy(), b.spawnWorkloadManager)
		b.workloadMgrSup.onCrash, b.workloadMgrSup.onRecovered = b.supervisedWorkerCallbacks("workload-manager", func() { b.setWorkloadMgr(nil) })
		if err := b.workloadMgrSup.Start(); err != nil {
			slog.Warn("workload-manager failed to start; continuing without cluster workload relay", "path", b.workloadMgrPath, "err", err)
			b.workloadMgrSup = nil
		} else {
			defer b.workloadMgrSup.Stop()
		}
	} else {
		slog.Info("workload-manager path not resolved; running without cluster workload relay")
	}

	// nvpair-manual-nodes and nvpair-cluster-manager are the remaining auxiliary
	// workers. Each is a
	// bidirectional JSON-RPC peer run under a supervisor (auto-restart +
	// crash surfacing); the broker demuxes their errors:* notifications
	// into the pipeline and relays their control-plane requests.
	b.manualNodesSup = b.startOptionalWorker("manual-nodes", b.manualNodesPath, b.spawnManualNodes, b.clearManualNodesState)
	if b.manualNodesSup != nil {
		defer b.manualNodesSup.Stop()
	}
	// nvpair-cluster-manager owns the node's cryptographic identity, the
	// trusted-node store, and the PIN-based pairing flow, and answers
	// cluster:* / nodes:* requests the broker relays.
	b.clusterMgrSup = b.startOptionalWorker("cluster-manager", b.clusterMgrPath, b.spawnClusterManager, func() { b.setClusterMgr(nil) })
	if b.clusterMgrSup != nil {
		defer b.clusterMgrSup.Stop()
	}
	// nvpair-job-scheduler ranks the discovered nodes least-loaded-first and emits
	// schedule:priority; the broker fans it out to the proxies. Non-fatal like
	// the other auxiliaries — absent it, the proxies keep their default order.
	b.schedulerSup = b.startOptionalWorker("scheduler", b.schedulerPath, b.spawnJobScheduler, func() { b.setScheduler(nil) })
	if b.schedulerSup != nil {
		defer b.schedulerSup.Stop()
	}

	if err := b.codec.Notify("app:ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("send app:ready: %w", err)
	}

	// Attach the change hook only AFTER app:ready is on the wire so a
	// scanner event arriving during startup can't race ahead of
	// app:ready and produce a discovery:nodes-changed before the
	// client has seen the broker come up. Detaching on the way out
	// keeps a last-millisecond scanner event from trying to push a
	// notification through a codec we're about to tear down. The hook
	// is always attached; whether it actually emits is gated by the
	// per-session subscription flag inside emitNodesChanged.
	b.store.SetOnChange(b.emitNodesChanged)
	defer b.store.SetOnChange(nil)

	err := b.readLoop(ctx)
	cancel()
	b.shutdownInferenceStack()
	return err
}

func (b *Broker) shutdownInferenceStack() {
	// Arm the shared budget here, at the first thing shutdown does, rather than
	// letting the first join arm it: everything below draws on the same window the
	// parent is waiting through, including the proxy joins and the engine StopAll
	// wait that both run before any deferred join.
	beginTeardown()

	// Stop ingress first so no new inference can arrive while engine-manager is
	// draining engines. supervisor.Stop uses the proxy's stdin-close/join path;
	// it never adds a parent-side kill timeout. One process holds every
	// engine's facade, so this closes all of them at once — the child drains
	// each facade before releasing the shared transport pool.
	if b.proxySup != nil {
		b.proxySup.Stop()
		for _, profile := range engineProxyProfiles {
			b.setEngineProxyHandle(profile, nil)
		}
	}
	b.prepareEngineManagerShutdown()
}

// prepareEngineManagerShutdown requests a synchronous engine shutdown from
// engine-manager and waits for it to finish. engine-manager bounds its own
// StopAll internally (see nvpair-engine-manager/lifecycle.go) and the peer.Call
// returns promptly with an error if it exits instead of responding, but neither
// of those is a bound the broker controls, and this step runs before any worker
// has been joined. Without a deadline of its own it could spend the parent's
// entire shutdown window here and leave nothing for the joins that follow — so it
// draws on the same teardown budget they do. A missing, failed, or slow
// engine-manager falls through to the stdin-close backstop.
func (b *Broker) prepareEngineManagerShutdown() {
	w := b.getEngineMgr()
	if w == nil {
		return
	}
	deadline := engineShutdownDeadline()
	slog.Info("requesting engine shutdown before teardown", "deadlineMs", deadline.Milliseconds())

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if _, rpcErr, err := w.CallNoTimeout(ctx, prepareEngineShutdownMethod, nil); err != nil || rpcErr != nil {
		slog.Warn("engine prepare-shutdown did not complete cleanly; relying on stdin-close backstop", "err", err, "rpcErr", rpcErr)
	}
}

// emitNodesChanged is the hook the discoveryStore invokes after every
// successful mutation. It is a no-op unless the peer has opted into the
// stream via discovery:subscribe — the discovery:nodes-changed stream
// is opt-in, so an unsubscribed (or never-subscribed) peer sees
// nothing. Once subscribed it defers to pushNodesChanged.
func (b *Broker) emitNodesChanged() {
	// The scheduler is fanned the node universe unconditionally (it's an
	// internal consumer, not a subscribing client), independent of whether the
	// external peer opted into discovery:nodes-changed.
	b.fanDiscoveryToScheduler()

	b.subMu.Lock()
	subscribed := b.subscribed
	b.subMu.Unlock()
	if !subscribed {
		return
	}
	b.pushNodesChanged()
}

// fanDiscoveryToScheduler pushes the current discovery snapshot to the scheduler
// child as a discovery:nodes-changed notification (same bare-array shape the
// client stream uses). A no-op when no scheduler is running.
func (b *Broker) fanDiscoveryToScheduler() {
	b.schedulerFeedMu.Lock()
	defer b.schedulerFeedMu.Unlock()

	sc := b.getScheduler()
	if sc == nil {
		return
	}
	if err := sc.Notify("discovery:nodes-changed", b.store.Snapshot()); err != nil {
		slog.Warn("fan discovery:nodes-changed to scheduler failed", "err", err)
	}
}

// pushNodesChanged takes a fresh snapshot and writes it to the peer as a
// bare-array discovery:nodes-changed notification — the payload shape is
// intentionally just []AvailableNode, not wrapped, per the external
// schema. Notify errors are logged but not fatal: the next mutation
// gets its own chance, and a client that's gone away will be torn down
// by the regular EOF path on the read side. It does NOT consult the
// subscription flag, so callers must gate on it themselves (the hook
// path via emitNodesChanged; the subscribe path because it has just set
// the flag true and wants an immediate baseline regardless of races).
func (b *Broker) pushNodesChanged() {
	if err := b.codec.Notify("discovery:nodes-changed", b.store.Snapshot()); err != nil {
		slog.Warn("emit discovery:nodes-changed failed", "err", err)
	}
}

// forwardProxyNotification is the hook startProxy invokes on the proxy
// reader goroutine for every notification the proxy emits. It re-emits the
// notification to the client as proxy:<method> (the proxy's native method
// name, prefixed) with params passed through unchanged — but only when the
// peer has opted in via proxy:subscribe. The stream is off by default, so
// an unsubscribed peer sees nothing. errors:report / errors:clear are
// routed into the nvpair-errors pipeline (the broker now supervises it)
// rather than dropped, so the proxy's upstream-unreachable reports reach
// the registry.
func (b *Broker) forwardProxyNotification(method string, params json.RawMessage) {
	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(), method, params)
}

func (b *Broker) forwardProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if generation != b.currentOllamaProxyGeneration() {
		return
	}
	// Strip the facade address before anything dispatches on the method: every
	// comparison below, and the errors relay in particular, matches bare names.
	method, addressed := facadeMethodFor(ollamaProxyProfile, method)
	if !addressed {
		return
	}
	// The alias warning is also the proxy's authoritative "bind did not win"
	// receipt. Release the candidate reservation before forwarding the sticky
	// warning so an existing Ollama owner can be adopted normally.
	if method == methodErrorsReport {
		var report errors.ServiceError
		if json.Unmarshal(params, &report) == nil && report.ID == ollamaHostAliasBlockedID {
			if !b.releaseOllamaHostAliasReservationForGeneration(generation) {
				return
			}
		}
	}
	if b.dispatchErrorsNotif(ollamaProxyProfile.ComponentName(), method, params) {
		return
	}
	// A process can win the port after startup preflight but before the facade
	// binds. Choose a safe fallback here, without calling back into this reader
	// goroutine; the backend move is still pending, so no engine state needs
	// rollback.
	//
	// This notification precedes the failing enable's response on the same
	// stream, so the port chosen here is what ollamaFallbackPort finds when the
	// enable retries. Deciding it here rather than there is deliberate: only
	// this path knows whether the managed facade has to be given up, and it
	// owns telling the user.
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" && !b.ollamaState().explicitSettings.Load() {
			switch {
			case b.ollamaState().managedFacade.Load() && ep.Port == managedOllamaFacadePort:
				b.blockManagedOllamaFacade("another process acquired the compatibility port during startup")
			default:
				// Without this the retry asks for the same port and the enable
				// fails identically, leaving the facade down for the life of
				// the process.
				fallback := b.setOllamaProxyFallback(ep.Port)
				slog.Warn("Ollama proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
			}
		}
	}
	// When the proxy announces a (re)bound port — notably its restored port
	// on startup — steer it clear of any running engine's port. Dispatched
	// on a separate goroutine: the corrective set-port round-trips through
	// this same proxy-reader goroutine, so calling it inline would deadlock.
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileProxyPortOnReady(rp.Port)
		}
	}
	b.forwardEngineProxyNotification(ollamaProxyProfile, method, params)
}

// routeNodeActivity relays a proxy's liveness report about a peer down to the
// node-scanner, which uses it to keep a node that is busy serving inference
// rather than evicting it for failing a probe it had no CPU to answer.
//
// It is consumed here rather than forwarded to clients: this is discovery input,
// not a proxy control-plane event, and no client has any use for it. The handoff
// is a non-blocking queue push (reportNodeActivity), so this stays on the proxy
// reader goroutine without letting a wedged scanner stall it.
func (b *Broker) routeNodeActivity(params json.RawMessage) {
	var p noderec.NodeActivityParams
	if err := json.Unmarshal(params, &p); err != nil || p.HostUUID == "" {
		return
	}
	sp := b.getScanner()
	if sp == nil {
		return
	}
	sp.reportNodeActivity(p.HostUUID, time.Now().Add(-clampActivityAge(p.MSSince)))
}

// clampActivityAge turns a reported age in milliseconds into a duration, bounded
// at both ends.
//
// The value crosses a process boundary, and the arithmetic is the fragile part: a
// large enough figure overflows the multiplication into a negative duration,
// which would place the observation in the FUTURE and vouch for the node forever.
// A negative input does the same directly. Neither is reachable from today's
// producers, which derive the value from time.Since — which is exactly why it is
// worth pinning here rather than relying on that staying true.
func clampActivityAge(msSince int64) time.Duration {
	if msSince <= 0 {
		return 0
	}
	// Compared before the multiplication, which is the operation that overflows.
	if msSince > int64(activityAgeCeiling/time.Millisecond) {
		return activityAgeCeiling
	}
	return time.Duration(msSince) * time.Millisecond
}

// activityAgeCeiling is far past any age the scanner still treats as fresh, so
// clamping to it cannot turn a stale report into a live one.
const activityAgeCeiling = time.Hour

// proxyWorkloadMethods is the set of workload notifications the broker
// recognizes coming off the proxy. The proxy only produces the lifecycle
// events today; workloads:remove is included so a future producer's
// removals route the same way rather than leaking onto the proxy:<event>
// client stream.
var proxyWorkloadMethods = map[string]bool{
	"workload:submitted": true,
	"workload:started":   true,
	"workload:completed": true,
	"workload:errored":   true,
	"workloads:remove":   true,
}

// routeProxyWorkload stamps the local node id onto a local-origin workload
// event, forwards it to the workload-manager's stdin for cluster broadcast,
// and echoes it to subscribed clients so a client's view includes local
// workloads alongside the peer-origin ones the manager relays. The proxy
// keeps emitting regardless of whether a manager is present, so the echo
// still happens with no manager supervised (the broadcast simply doesn't).
func (b *Broker) routeProxyWorkload(method string, params json.RawMessage) {
	stamped := stampNodeID(params, b.nodeID)
	// Update the authoritative store (and echo to clients / fan to the
	// scheduler) BEFORE forwarding to the workload-manager, so the store is
	// never staler than what the manager has broadcast. Rehydration snapshots
	// the store on a manager restart; a forward-first ordering could let the
	// manager broadcast a terminal, then a restart replay the store's not-yet-
	// updated running over it. A lifecycle event becomes a workloads:upsert (the
	// same translation the manager applies to peer-origin lifecycle events, spec
	// 7.0); a removal stays a removal.
	if method == "workloads:remove" {
		b.emitWorkloadEvent("workloads:remove", stamped)
	} else {
		b.emitWorkloadEvent("workloads:upsert", stamped)
	}
	if wm := b.getWorkloadMgr(); wm != nil {
		if err := wm.Forward(method, stamped); err != nil {
			slog.Warn("failed to forward workload event to workload-manager", "method", method, "err", err)
		}
	} else {
		slog.Debug("no workload-manager supervised; workload event not broadcast", "method", method)
	}
}

// stampNodeID fills in the origin node id on a workload notification when
// the producer left it empty, the way the proxy does (it has no canonical
// node id). Lifecycle events carry the id inside params.workloadInfo;
// removals carry it at the top level. The params are manipulated as generic
// JSON so the broker doesn't duplicate (and risk drifting from) the
// Workload schema, and an already-populated id is left untouched. On any
// shape it doesn't recognize it forwards the params unchanged.
func stampNodeID(params json.RawMessage, nodeID string) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(params, &envelope); err != nil {
		return params
	}
	if wiRaw, ok := envelope["workloadInfo"]; ok {
		var wi map[string]json.RawMessage
		if err := json.Unmarshal(wiRaw, &wi); err != nil {
			return params
		}
		// The Workload's origin lives under originatedFrom (spec 6); the
		// removal disambiguator (else branch) carries the same originatedFrom
		// field at the top level.
		if !setNodeIDIfEmpty(wi, "originatedFrom", nodeID) {
			return params
		}
		newWI, err := json.Marshal(wi)
		if err != nil {
			return params
		}
		envelope["workloadInfo"] = newWI
	} else if !setNodeIDIfEmpty(envelope, "originatedFrom", nodeID) {
		return params
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return params
	}
	return out
}

// setNodeIDIfEmpty sets m[key] to nodeID when it is absent or an empty
// string, returning whether it modified the map. Both the Workload origin
// (nested under workloadInfo) and the removal disambiguator (top level) use
// the "originatedFrom" key.
func setNodeIDIfEmpty(m map[string]json.RawMessage, key, nodeID string) bool {
	if cur, ok := m[key]; ok {
		var s string
		if json.Unmarshal(cur, &s) == nil && s != "" {
			return false
		}
	}
	raw, err := json.Marshal(nodeID)
	if err != nil {
		return false
	}
	m[key] = raw
	return true
}

func (b *Broker) readLoop(ctx context.Context) error {
	// codec.Read() blocks on stdin, so we run it on its own goroutine and
	// select against ctx.Done(). Otherwise a SIGINT/SIGTERM (which cancels
	// ctx) would leave us parked in Read until a line or EOF arrived on
	// stdin — Serve would never return, its deferred worker teardown would
	// never run, and the broker would appear to hang on Ctrl+C.
	type readResult struct {
		msg *Message
		err error
	}
	reads := make(chan readResult)
	go func() {
		for {
			msg, err := b.codec.Read()
			select {
			case reads <- readResult{msg: msg, err: err}:
			case <-ctx.Done():
				return
			}
			// EOF is terminal (stream closed); other errors are per-line
			// (e.g. a bad JSON frame) and the next Read advances past them.
			if err == io.EOF {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-reads:
			if r.err != nil {
				if r.err == io.EOF || ctx.Err() != nil {
					return nil
				}
				slog.Warn("JSON-RPC read error", "err", r.err)
				continue
			}
			b.handleMessage(r.msg)
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

// forwardLogLevel pushes an already-validated log level down to every
// running child subprocess over its stdin. Each child applies it through
// the shared applog stdin handler. Failures are logged per-child but
// never propagated: a child that won't take the new level (e.g. its pipe
// is mid-teardown, or it never started) must not fail the caller's
// log/set-level request, which has already succeeded for the broker
// itself. Each handle is read through its workersMu-guarded getter because
// a supervisor may swap it (on a restart) concurrently with this call.
func (b *Broker) forwardLogLevel(level string) {
	if s := b.getScanner(); s != nil {
		if err := s.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to scanner", "err", err)
		}
	}
	if n := b.getNodeInfo(); n != nil {
		if err := n.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to node-info", "err", err)
		}
	}
	// One iteration over distinct handles, not one per engine: every engine's
	// facade lives in the same process now, so both engine slots resolve to the
	// same handle and a per-engine loop would send this process-scoped
	// notification twice.
	for _, target := range b.livePriorityTargets() {
		if err := target.proxy.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to proxy", "err", err)
		}
	}
	if wm := b.getWorkloadMgr(); wm != nil {
		if err := wm.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to workload-manager", "err", err)
		}
	}
	if ep := b.getErrors(); ep != nil {
		if err := ep.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to nvpair-errors", "err", err)
		}
	}
	if em := b.getEngineMgr(); em != nil {
		if err := em.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to engine-manager", "err", err)
		}
	}
	if mn := b.getManualNodes(); mn != nil {
		if err := mn.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to manual-nodes", "err", err)
		}
	}
	if st := b.getSettings(); st != nil {
		if err := st.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to settings", "err", err)
		}
	}
	if cm := b.getClusterMgr(); cm != nil {
		if err := cm.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to cluster-manager", "err", err)
		}
	}
	if sc := b.getScheduler(); sc != nil {
		if err := sc.SetLogLevel(level); err != nil {
			slog.Warn("failed to forward log/set-level to scheduler", "err", err)
		}
	}
}

// forwardClusterManagerNotification is the hook startClusterManager invokes on
// the manager's reader goroutine for every notification it emits. The
// cluster-manager's events (cluster:invite-received, cluster:identity-changed,
// nodes:changed, ...) are low-volume, important push events with no opt-in
// stream, so they are relayed to the client verbatim. errors:report /
// errors:clear are routed into the nvpair-errors pipeline (the broker now
// supervises it) rather than dropped.
func (b *Broker) forwardClusterManagerNotification(method string, params json.RawMessage) {
	if b.dispatchErrorsNotif("cluster-manager", method, params) {
		return
	}
	if err := b.codec.Notify(method, params); err != nil {
		slog.Warn("forward cluster-manager notification failed", "method", method, "err", err)
	}
	// When this node's cluster identity is (re)established — cluster:create,
	// adopt-on-join, or cleared by cluster:leave — persist it to nvpair-node-settings
	// (the durable home restoreClusterIdentity reads on next startup) and converge
	// this node's advertised cluster identity. The cluster-scoped workers pick the
	// membership change up themselves; see applyClusterIdentityChange.
	if method == "cluster:identity-changed" {
		var id struct {
			ClusterID           string `json:"clusterId"`
			ClusterFriendlyName string `json:"clusterFriendlyName"`
		}
		if err := json.Unmarshal(params, &id); err != nil {
			slog.Warn("cluster:identity-changed had undecodable params; not persisting", "err", err)
		} else {
			b.persistClusterIdentity(id.ClusterID, id.ClusterFriendlyName)
		}
		b.applyClusterIdentityChange()
	}
	// A pairing or removal changed the pin set. The cluster-scoped workers read
	// their gates live and need nothing, but the node-scanner caches a per-peer
	// trusted annotation it can only otherwise refresh when that peer's mDNS
	// record moves — which a peer already advertising when we joined never does.
	if method == "cluster:trust-changed" {
		b.applyClusterTrustChange()
	}
}

// applyClusterTrustChange re-derives the scanner's per-peer trust annotation
// after nvpair-cluster-manager reports a pin-set change. Fire-and-forget on its
// own goroutine, for the same reason as applyClusterIdentityChange: it is a
// bounded-but-blocking RPC and the caller is the cluster-manager's reader.
//
// node-info is pushed here too, not only from applyClusterIdentityChange: a pin
// on its own can decide membership (a cluster dir predating admission.json is
// clustered on pins alone — see clustertrust.Mesh), so this event can change the
// principal we publish without any identity change being reported. Peers now
// read that value over HTTP in preference to our mDNS record, so it must not have
// fewer refresh triggers than the record does — otherwise a peer converges on a
// principal we have already stopped advertising.
func (b *Broker) applyClusterTrustChange() {
	slog.Info("cluster pin set changed; re-deriving peer trust")
	if sc := b.getScanner(); sc != nil {
		go sc.reloadTrust()
	}
	go b.pushClusterIdentityToNodeInfo()
}

// forwardWorkloadManagerNotification is the hook startWorkloadManager
// invokes on the manager's reader goroutine for every notification it emits
// on stdout. The manager translates peer-origin lifecycle events into
// workloads:upsert and peer-origin removals into workloads:remove (spec
// 7.0); those are relayed to subscribed clients verbatim. Its startup
// "ready" (and anything else) is internal and dropped.
func (b *Broker) forwardWorkloadManagerNotification(method string, params json.RawMessage) {
	switch method {
	case "workloads:upsert", "workloads:remove":
		b.emitWorkloadEvent(method, params)
	default:
		slog.Debug("ignoring workload-manager notification", "method", method)
	}
}

// emitWorkloadEvent applies a workloads:* notification to the authoritative
// store and, only when it is a real forward change, fans it to the scheduler
// and re-emits it to a subscribed client. Both the inbound relay (peer-origin
// events off the manager's stdout) and the local echo (local-origin proxy
// workloads) funnel through here. Dropping stale/regressive upserts at the
// store is what stops an out-of-order "running" (arriving after a workload's
// terminal event) from resurrecting it on the client stream and in the
// scheduler's view. The stream is off by default, so an unsubscribed client
// still sees nothing — but the store is always kept current.
//
// The whole apply→fan→notify sequence is serialized by workloadEmitMu so the
// client-visible order matches the applied order even when the node-loss sweep
// (a separate goroutine) races a relayed event for the same workload.
func (b *Broker) emitWorkloadEvent(method string, params json.RawMessage) {
	b.emitWorkloadEventProvenance(method, params, false, noSightingGuard)
}

// emitWorkloadEventInferred is emitWorkloadEvent for a locally-inferred
// transition — the node-loss sweep guessing a departed node's jobs failed. It
// records the state as inferred so a later authoritative event from the origin
// (e.g. the node was only briefly unreachable) reconciles it away.
func (b *Broker) emitWorkloadEventInferred(method string, params json.RawMessage) {
	b.emitWorkloadEventProvenance(method, params, true, noSightingGuard)
}

// emitWorkloadEventStale is emitWorkloadEventInferred for the staleness sweep,
// which selects its candidates before it applies them. seenAt is the sighting the
// decision was based on: if the origin has spoken about this workload since, the
// guess is already known to be wrong and is dropped rather than briefly showing
// running work as failed.
func (b *Broker) emitWorkloadEventStale(method string, params json.RawMessage, seenAt time.Time) {
	b.emitWorkloadEventProvenance(method, params, true, seenAt)
}

// noSightingGuard means "apply unconditionally" — every caller except the
// staleness sweep. The zero time is safe as the sentinel because a stored record
// always carries a real sighting.
var noSightingGuard time.Time

func (b *Broker) emitWorkloadEventProvenance(method string, params json.RawMessage, inferred bool, unchangedSince time.Time) {
	b.workloadEmitMu.Lock()
	defer b.workloadEmitMu.Unlock()

	if !b.applyWorkloadEvent(method, params, inferred, unchangedSince) {
		return
	}

	// The scheduler is fanned every real transition (independent of the
	// external client's opt-in) so it can keep its own catalog.
	b.fanWorkloadToScheduler(method, params)

	b.workloadsMu.Lock()
	subscribed := b.workloadsSubscribed
	b.workloadsMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify(method, params); err != nil {
		slog.Warn("emit workloads event failed", "method", method, "err", err)
	}
}

// fanWorkloadToScheduler forwards a workloads:upsert/remove notification to the
// scheduler child verbatim. A no-op when no scheduler is running.
func (b *Broker) fanWorkloadToScheduler(method string, params json.RawMessage) {
	b.schedulerFeedMu.Lock()
	defer b.schedulerFeedMu.Unlock()

	sc := b.getScheduler()
	if sc == nil {
		return
	}
	if err := sc.Notify(method, params); err != nil {
		slog.Warn("fan workload event to scheduler failed", "method", method, "err", err)
	}
}

// workloadsInitialResult is the workloads:get-initial response: the store's
// full-fidelity snapshot as a list of workloadInfo objects (the same shape
// carried inside each workloads:upsert), so a client can key them however it
// likes.
type workloadsInitialResult struct {
	Workloads []json.RawMessage `json:"workloads"`
}

// applyWorkloadEvent folds a workloads:* notification into the authoritative
// store and reports whether it was a real change worth propagating. A
// workloads:upsert is merged by the store's monotonic rule (stale/regressive
// or duplicate events return false, so they're neither fanned nor forwarded);
// a workloads:remove drops the entry and always propagates. Malformed or
// id-less params are ignored (false). Callers hold workloadEmitMu.
func (b *Broker) applyWorkloadEvent(method string, params json.RawMessage, inferred bool, unchangedSince time.Time) bool {
	switch method {
	case "workloads:upsert":
		var env struct {
			WorkloadInfo json.RawMessage `json:"workloadInfo"`
		}
		if err := json.Unmarshal(params, &env); err != nil || len(env.WorkloadInfo) == 0 {
			return false
		}
		in, ok := workloadstore.ParseIncoming(env.WorkloadInfo)
		if !ok {
			return false
		}
		if inferred {
			if !unchangedSince.IsZero() {
				return b.workloads.ApplyInferredUnchangedSince(in, unchangedSince)
			}
			return b.workloads.ApplyInferred(in)
		}
		return b.workloads.Apply(in)
	case "workloads:remove":
		var rm struct {
			WorkloadID     string `json:"workloadId"`
			OriginatedFrom string `json:"originatedFrom"`
		}
		if err := json.Unmarshal(params, &rm); err != nil || rm.WorkloadID == "" {
			return false
		}
		b.workloads.Remove(rm.OriginatedFrom, rm.WorkloadID)
		return true
	default:
		return false
	}
}

// failWorkloadsForNode marks every non-terminal workload pinned to a node that
// just dropped out of discovery as failed. A node leaves the directory only
// after the scanner's miss-threshold + TCP-probe liveness guard has given up on
// it — a full minute of unbroken silence, a failed probe on every scan of the
// last three quarters of it, and no inference traffic from the node in that time
// either, not a transient mDNS miss — so this is a strong
// "the node is really gone" signal. That node's own proxy can no
// longer emit a terminal workload:completed/errored (its process or host is
// gone), so the broker synthesizes a workloads:upsert transition to "failed"
// for each affected workload — clearing the stale "in progress" line while
// keeping the entry in clients' history (we upsert, never remove).
//
// A workload is affected if the departed node is either its origin
// (originatedFrom — the node that submitted it) or its executor (scheduledOn —
// the node that was running it). nodeUUID is the stable per-host UUID; the
// broker stamps that same value as originatedFrom and the proxy records it as
// scheduledOn, so the three correlate directly regardless of a
// hostname change. nodeName is the display hostname, used only in the error
// message. The caller fires this only after the node's final directory claim is
// gone (a surviving manual claim means the node is still present).
//
// The transition is applied as INFERRED (emitWorkloadEventInferred): a guess
// this node made from a discovery miss, not the origin's authoritative truth.
// That provenance is what makes this safe against a wrong guess — the sweep is
// deliberately local (does NOT re-broadcast to peers), and it is reconciled by
// the origin's authoritative re-sync (the workload-manager heartbeat re-asserts
// each node's own active + recently-terminal workloads). So if node B evicts
// node A over a transient partition and infers A's jobs failed, A's next re-sync
// carries its authoritative state and the store's provenance-aware merge
// overrides B's inferred guess — un-sticking it. If A is truly gone, nothing
// overrides the guess and it stands. The origin is the single writer and the
// convergence authority; this sweep only supplies a fast, provisional local
// answer in the gap before the next re-sync. It relies on the scanner's
// eviction being a reasonable "offline" signal — a false positive is corrected,
// not permanent, but until the correcting re-sync arrives (≤ a couple of
// heartbeats) a briefly-partitioned node's jobs show as failed here.
func (b *Broker) failWorkloadsForNode(nodeUUID, nodeName string) {
	if nodeUUID == "" {
		return
	}
	now := time.Now()
	// Match workloads by the stable HostUUID (what the proxy stamps as
	// originatedFrom / scheduledOn); the hostname is display only, so
	// use it in the error message but fall back to the UUID if it's absent.
	display := nodeName
	if display == "" {
		display = nodeUUID
	}
	errMsg := fmt.Sprintf("node %s went offline", display)

	// Snapshot the still-live workloads pinned to this node, then synthesize a
	// terminal transition for each, applied as inferred. An inferred failed
	// marks the running record terminal locally (so it leaves the next
	// ActiveForNode sweep), but yields to the origin's authoritative re-sync if
	// the node was only briefly unreachable.
	var failed []json.RawMessage
	for _, wl := range b.workloads.ActiveForNode(nodeUUID) {
		if params := markWorkloadFailed(wl.Info, now, errMsg); params != nil {
			failed = append(failed, params)
		}
	}

	if len(failed) == 0 {
		return
	}
	slog.Info("marking in-flight workloads failed for departed node", "node", display, "uuid", nodeUUID, "count", len(failed))
	for _, params := range failed {
		// Inferred: a guess this node made from a discovery miss. If the node
		// was only briefly unreachable, its authoritative re-sync reconciles
		// these back to their real state; if it's truly gone, the guess stands.
		b.emitWorkloadEventInferred("workloads:upsert", params)
	}
}

// Workload staleness sweep tuning.
//
// The origin's workload-manager re-asserts every one of its own non-terminal
// workloads to every peer on a fixed heartbeat (30s at the time of writing;
// duplicated here as a number rather than imported because it is a private
// constant of that binary). So for a workload we believe is running on another
// node, continued silence across many consecutive heartbeats means the origin no
// longer considers it active — it finished and we lost every copy of the
// terminal, or the origin stopped running.
//
// The window is deliberately generous: ten missed re-assertions rather than two,
// because the cost of waiting is a stale line for a few minutes while the cost of
// being hasty is briefly mislabelling a genuinely running remote job. A wrong
// guess is self-correcting either way — the sweep applies an INFERRED terminal,
// which the origin's next authoritative event overrides.
const (
	workloadOriginSilenceTimeout = 5 * time.Minute
	workloadStaleSweepInterval   = 60 * time.Second
)

// runStaleWorkloadSweep periodically retires remote-origin workloads whose origin
// has gone silent about them. It is the backstop that bounds how long a lost
// terminal event can display as in-flight: every other path in the workload
// pipeline is push-only, so without this a single dropped completed/errored
// leaves the line running until the process restarts.
func (b *Broker) runStaleWorkloadSweep(ctx context.Context) {
	ticker := time.NewTicker(workloadStaleSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.failStaleForeignWorkloads(workloadOriginSilenceTimeout)
		}
	}
}

// failStaleForeignWorkloads marks every remote-origin non-terminal workload the
// origin has not re-asserted within workloadOriginSilenceTimeout as failed.
//
// This is the same synthesized transition the node-loss sweep applies, for the
// same reason and with the same provenance, but triggered by a different signal:
// failWorkloadsForNode fires when a node leaves discovery, whereas this fires
// when a node is still present yet has stopped mentioning a workload we think it
// is running. Neither can substitute for the other — a peer whose delivery to us
// is failing stays happily in discovery.
// staleAfter is a parameter rather than the constant so a test can drive the
// boundary without waiting on the wall clock.
func (b *Broker) failStaleForeignWorkloads(staleAfter time.Duration) {
	stale := b.workloads.StaleForeign(b.nodeID, staleAfter)
	if len(stale) == 0 {
		return
	}
	now := time.Now()
	names := b.nodeDisplayNames()
	for _, wl := range stale {
		display := wl.Origin
		if name, ok := names[wl.Origin]; ok && name != "" {
			display = name
		}
		params := markWorkloadFailed(wl.Info, now, fmt.Sprintf("node %s stopped reporting this workload", display))
		if params == nil {
			continue
		}
		slog.Info("retiring workload the origin stopped reporting",
			"id", wl.ID, "origin", wl.Origin, "state", wl.State,
			"silentFor", now.Sub(wl.LastSeen))
		// Inferred, so the origin's next authoritative event wins if it turns out
		// to still be running and merely unable to reach us — and guarded on the
		// sighting this decision came from, so an origin that spoke up between the
		// selection above and this apply is not overwritten by a guess that is
		// already known to be wrong.
		b.emitWorkloadEventStale("workloads:upsert", params, wl.LastSeen)
	}
}

// nodeDisplayNames maps node UUID to advertised hostname for user-facing error
// text. Built once per sweep rather than per record: Snapshot projects and sorts
// the whole directory on every call, and a sweep can touch many records at once.
// A UUID missing from the map (a peer that has since left) falls back to itself.
func (b *Broker) nodeDisplayNames() map[string]string {
	if b.store == nil {
		return nil
	}
	nodes := b.store.Snapshot()
	names := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.HostUUID != "" && n.Name != "" {
			names[n.HostUUID] = n.Name
		}
	}
	return names
}

// markWorkloadFailed rewrites a stored workloadInfo into a terminal "failed"
// transition and wraps it in the workloads:upsert params envelope. It edits
// the object as generic JSON — setting state/completedAt/error and leaving
// every other field (id, model, engine, originatedFrom, scheduledOn,
// createdAt, startedAt, requesterId) untouched — so the broker never
// duplicates the Workload schema. Returns nil if the stored info can't be
// parsed or re-marshaled.
//
// It takes a time.Time and converts to epoch ms itself: this is the only place
// that serializes the Workload wire format, so it is the only place that should
// know the format's representation. Callers stay in time.Time.
func markWorkloadFailed(info json.RawMessage, completedAt time.Time, errMsg string) json.RawMessage {
	var wi map[string]json.RawMessage
	if err := json.Unmarshal(info, &wi); err != nil {
		return nil
	}
	set := func(key string, v any) {
		if raw, err := json.Marshal(v); err == nil {
			wi[key] = raw
		}
	}
	set("state", "failed")
	set("completedAt", completedAt.UnixMilli())
	set("error", errMsg)
	newWI, err := json.Marshal(wi)
	if err != nil {
		return nil
	}
	env, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": newWI})
	if err != nil {
		return nil
	}
	return env
}

func (b *Broker) handleMessage(msg *Message) {
	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				b.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			b.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
			// The broker fans the (validated) level out to every child it
			// supervises so a single log/set-level call adjusts the whole
			// process tree, not just the broker. Done after the caller's
			// response is on the wire; child forwarding is best-effort.
			b.forwardLogLevel(resolved)
		}
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			// A client can inject an errors:report the same way a supervised
			// worker emits one on its stdout — surfacing its own operational
			// errors (e.g. a model-pull failure the engine itself doesn't
			// report) through the shared registry that drives errors:update.
			// dispatchErrorsNotif stamps the origin nodeId / timestamp
			// exactly as it does for workers. Every other inbound
			// notification is dropped.
			if msg.Method == methodErrorsReport && b.dispatchErrorsNotif("client", msg.Method, msg.Params) {
				return
			}
			slog.Debug("ignoring incoming notification", "method", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "engine:get-settings", "engine:preview-settings", "engine:apply-settings":
		go b.handleEngineSettings(msg)
	case "ping":
		if err := b.codec.Respond(msg.ID, PingResult{
			Pong:     true,
			Version:  Version,
			UptimeMS: time.Since(b.startedAt).Milliseconds(),
		}); err != nil {
			log.Printf("failed to respond to ping: %v", err)
		}

	case "version":
		if err := b.codec.Respond(msg.ID, VersionResult{Version: Version}); err != nil {
			log.Printf("failed to respond to version: %v", err)
		}

	case "discovery:get-nodes":
		if err := b.codec.Respond(msg.ID, GetNodesResult{Nodes: b.store.Snapshot()}); err != nil {
			log.Printf("failed to respond to discovery:get-nodes: %v", err)
		}

	case "discovery:subscribe":
		b.subMu.Lock()
		wasSubscribed := b.subscribed
		b.subscribed = true
		b.subMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
			log.Printf("failed to respond to discovery:subscribe: %v", err)
		}
		// On a fresh subscription, push the current snapshot once so the
		// subscriber gets an immediate baseline without a separate
		// discovery:get-nodes round-trip. Sent AFTER the response so the
		// caller sees the ack first. A redundant re-subscribe is a no-op
		// and does not re-emit — the stream is already flowing.
		if !wasSubscribed {
			b.pushNodesChanged()
		}

	case "discovery:unsubscribe":
		b.subMu.Lock()
		b.subscribed = false
		b.subMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
			log.Printf("failed to respond to discovery:unsubscribe: %v", err)
		}

	case "ollama-proxy:get-status":
		// Answered locally from the proxy handle's captured state — no
		// round-trip to the proxy. When no proxy is supervised the
		// zero value ({ready:false, port:0}) is a valid "not available"
		// answer, so the method never errors.
		var result ProxyStatusResult
		if p := b.getProxy(); p != nil {
			ready, port := p.Status(ollamaProxyProfile.Name)
			result.Ready = ready
			result.Port = port
		}
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to respond to proxy:get-status: %v", err)
		}

	case "ollama-proxy:subscribe":
		b.proxyMu.Lock()
		wasSubscribed := b.setEngineProxySubscribed(ollamaProxyProfile, true)
		b.proxyMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
			log.Printf("failed to respond to proxy:subscribe: %v", err)
		}
		// On a fresh subscription replay the proxy's last "ready" payload
		// (if it has come up) as a baseline proxy:ready, so a subscriber
		// learns the port without a separate proxy:get-status. Sent after
		// the ack. A redundant re-subscribe doesn't re-emit.
		if !wasSubscribed {
			if p := b.getProxy(); p != nil {
				if rp := p.ReadyParams(ollamaProxyProfile.Name); rp != nil {
					if err := b.codec.Notify("ollama-proxy:ready", rp); err != nil {
						slog.Warn("emit baseline proxy:ready failed", "err", err)
					}
				}
			}
		}

	case "ollama-proxy:unsubscribe":
		b.proxyMu.Lock()
		b.setEngineProxySubscribed(ollamaProxyProfile, false)
		b.proxyMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
			log.Printf("failed to respond to proxy:unsubscribe: %v", err)
		}

	case "engine:set-port":
		go b.handleSettingsPortRPC(msg, "")
	case "ollama-proxy:set-port":
		go b.handleSettingsPortRPC(msg, "ollama")
	case "lmstudio-proxy:set-port":
		go b.handleSettingsPortRPC(msg, "lmstudio")

	case "lmstudio-proxy:get-status":
		// Answered locally from the lmstudio-proxy handle's captured state,
		// mirroring proxy:get-status. Zero value when none is supervised.
		var result ProxyStatusResult
		if p := b.getLMStudioProxy(); p != nil {
			ready, port := p.Status(lmstudioProxyProfile.Name)
			result.Ready = ready
			result.Port = port
		}
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to respond to lmstudio-proxy:get-status: %v", err)
		}

	case "lmstudio-proxy:subscribe":
		b.proxyMu.Lock()
		wasSubscribed := b.setEngineProxySubscribed(lmstudioProxyProfile, true)
		b.proxyMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
			log.Printf("failed to respond to lmstudio-proxy:subscribe: %v", err)
		}
		if !wasSubscribed {
			if p := b.getLMStudioProxy(); p != nil {
				if rp := p.ReadyParams(lmstudioProxyProfile.Name); rp != nil {
					if err := b.codec.Notify("lmstudio-proxy:ready", rp); err != nil {
						slog.Warn("emit baseline lmstudio-proxy:ready failed", "err", err)
					}
				}
			}
		}

	case "lmstudio-proxy:unsubscribe":
		b.proxyMu.Lock()
		b.setEngineProxySubscribed(lmstudioProxyProfile, false)
		b.proxyMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
			log.Printf("failed to respond to lmstudio-proxy:unsubscribe: %v", err)
		}

	case "workloads:subscribe":
		b.workloadsMu.Lock()
		b.workloadsSubscribed = true
		b.workloadsMu.Unlock()
		// No baseline: the broker keeps no workload catalog (it's a relay,
		// not the source of truth), so there's nothing to replay. The
		// subscriber sees events from the next local-origin or peer-origin
		// transition onward.
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
			log.Printf("failed to respond to workloads:subscribe: %v", err)
		}

	case "workloads:unsubscribe":
		b.workloadsMu.Lock()
		b.workloadsSubscribed = false
		b.workloadsMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
			log.Printf("failed to respond to workloads:unsubscribe: %v", err)
		}

	case "workloads:get-initial":
		// Authoritative baseline from the durable store: every current +
		// historic workload as its full workloadInfo, so a (re)connecting or
		// late-subscribing client can seed its view without waiting for the
		// next transition. Ordered by createdAt (see Store.Snapshot).
		snapshot := b.workloads.Snapshot()
		if snapshot == nil {
			snapshot = []json.RawMessage{}
		}
		if err := b.codec.Respond(msg.ID, workloadsInitialResult{Workloads: snapshot}); err != nil {
			log.Printf("failed to respond to workloads:get-initial: %v", err)
		}

	case "node/add", "node/remove", "nodes/list":
		b.relayToManualNodes(msg)

	case "engine:set-reserved-port", "internal:set-reserved-port":
		if err := b.codec.RespondError(msg.ID, -32601, "method not found: "+msg.Method); err != nil {
			log.Printf("failed to reject private engine method %s: %v", msg.Method, err)
		}

	// Reached only because the default branch relays every other engine:*
	// method straight to the engine manager. Configuring a launch that way
	// would skip the journal entry, the port reservation, and the proxy
	// rebind that engine:apply-settings owns, leaving the broker's recorded
	// settings describing an engine it no longer matches.
	case "engine:configure-launch":
		if err := b.codec.RespondError(msg.ID, -32601, "use engine:apply-settings for launch configuration"); err != nil {
			log.Printf("failed to reject direct %s: %v", msg.Method, err)
		}

	case "engine:subscribe":
		b.engineMu.Lock()
		b.engineSubscribed = true
		b.engineMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: true}); err != nil {
			log.Printf("failed to respond to engine:subscribe: %v", err)
		}

	case "engine:unsubscribe":
		b.engineMu.Lock()
		b.engineSubscribed = false
		b.engineMu.Unlock()
		if err := b.codec.Respond(msg.ID, SubscriptionResult{Subscribed: false}); err != nil {
			log.Printf("failed to respond to engine:unsubscribe: %v", err)
		}

	case methodErrorsGetInitial:
		b.handleErrorsGetInitial(msg)

	case methodErrorsClear:
		b.handleErrorsClear(msg)

	case methodErrorsReport:
		b.handleErrorsReport(msg)

	case "shutdown":
		if err := b.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		slog.Info("shutdown requested via JSON-RPC")
		b.cancel()

	default:
		// Any remaining method under an engine's <component>: prefix is
		// relayed verbatim to that engine's proxy (the reserved broker-local
		// ones — get-status and the subscription methods — are handled by
		// their own cases above). This makes the broker a thin pass-through
		// for each proxy's whole control plane without enumerating methods.
		//
		// The prefixes are the engines' ComponentName values, so this loop
		// gains a new engine for free rather than needing another branch.
		if profile, ok := engineProxyProfileForMethod(msg.Method); ok {
			b.relayToEngineProxy(profile, msg)
			return
		}
		if strings.HasPrefix(msg.Method, "engine:") {
			b.relayToEngine(msg)
			return
		}
		if strings.HasPrefix(msg.Method, "settings/") {
			b.relayToSettings(msg)
			return
		}
		// cluster:* and nodes:* are the cluster-manager's namespaces; relay
		// them verbatim to nvpair-cluster-manager.
		if strings.HasPrefix(msg.Method, "cluster:") || strings.HasPrefix(msg.Method, "nodes:") {
			b.relayToClusterManager(msg)
			return
		}
		if err := b.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}

// relayToEngine forwards an engine:<method> request to nvpair-engine-manager
// and relays its eventual response to the client. engine-manager's methods
// are already engine:-prefixed, so the method is forwarded verbatim (no
// prefix translation). Unlike the proxy relay it imposes no broker-side
// timeout: engine lifecycle ops (install, model pull, ...) run for minutes
// and report progress via push events, so the broker waits for the real
// response asynchronously rather than fabricating a timeout — meanwhile
// other client requests keep being served on the read-loop goroutine.
func (b *Broker) relayToEngine(msg *Message) {
	go b.relayToEngineNow(msg)
}

func (b *Broker) relayToEngineNow(msg *Message) {
	if needsOllamaPortGate(msg.Method, msg.Params) && b.ollamaFacadeIsPendingBackend() {
		go func() {
			select {
			case <-b.ollamaPortReady:
				// Recovery opens the general port gate before a best-effort live
				// rebind can finish. Never probe if the proxy still owns :11434.
				if b.ollamaFacadeIsPendingBackend() {
					_ = b.codec.RespondError(msg.ID, -32000, "Ollama port setup is still reconciling; retry")
					return
				}
				b.relayToEngine(msg)
			case <-time.After(rpcWorkerCallTimeout):
				_ = b.codec.RespondError(msg.ID, -32000, "Ollama port setup did not finish; retry")
			}
		}()
		return
	}
	if needsLMStudioPortGate(msg.Method, msg.Params) && b.lmstudioPortOwnershipPending() {
		go func() {
			select {
			case <-b.lmstudioPortReady:
				b.relayToEngine(msg)
			case <-time.After(rpcWorkerCallTimeout):
				_ = b.codec.RespondError(msg.ID, -32000, "LM Studio port setup did not finish; retry")
			}
		}()
		return
	}

	requestedEngine, requestedPort, isEnginePortAssignment := enginePortAssignmentRequest(msg.Method, msg.Params)
	if isEnginePortAssignment || msg.Method == "engine:stop" || msg.Method == "engine:start" || msg.Method == "engine:restart" {
		b.engineConfigMu.Lock()
		defer b.engineConfigMu.Unlock()
		defer b.reconcileLegacySettingsLocked()
	}
	isOllamaPortAssignment := isEnginePortAssignment && requestedEngine == "ollama"
	if isEnginePortAssignment && b.rejectOllamaHostAliasPort(msg, requestedPort, requestedEngine) {
		return
	}
	if isOllamaPortAssignment && requestedPort == managedOllamaFacadePort && b.ollamaState().managedFacade.Load() {
		if err := b.codec.RespondError(msg.ID, -32000, "port 11434 is reserved by the managed Ollama proxy; choose another backend port or disable managed port ownership and restart NVPAIR"); err != nil {
			log.Printf("failed to reject conflicting Ollama port assignment: %v", err)
		}
		return
	}
	requestedLMStudioPort, isLMStudioSetPort := lmstudioSetPortRequest(msg.Method, msg.Params)
	if isLMStudioSetPort && requestedLMStudioPort == managedLMStudioFacadePort && b.lmstudioState().managedFacade.Load() {
		if err := b.codec.RespondError(msg.ID, -32000, "port 1234 is reserved by the managed LM Studio proxy; choose another backend port or disable managed port ownership and restart NVPAIR"); err != nil {
			log.Printf("failed to reject conflicting LM Studio engine:set-port: %v", err)
		}
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "engine-manager not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}
	// Capture the client's id/method for the async response closure; the
	// read loop allocates a fresh Message per request so these are stable.
	id := msg.ID
	method := msg.Method
	result, rpcErr, err := em.CallNoTimeout(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if e := b.codec.RespondError(id, -32000, fmt.Sprintf("engine call failed: %v", err)); e != nil {
			log.Printf("failed to relay engine error for %s: %v", method, e)
		}
	case rpcErr != nil:
		if e := b.codec.RespondError(id, rpcErr.Code, rpcErr.Message); e != nil {
			log.Printf("failed to relay engine error for %s: %v", method, e)
		}
	default:
		if isOllamaPortAssignment {
			var status struct {
				Port int `json:"port"`
			}
			if json.Unmarshal(result, &status) == nil && status.Port > 0 {
				b.ollamaState().backendPort.Store(int32(status.Port))
			}
		}
		if isLMStudioSetPort {
			b.lmstudioState().backendPort.Store(int32(requestedLMStudioPort))
		}
		if e := b.codec.Respond(id, result); e != nil {
			log.Printf("failed to relay engine result for %s: %v", method, e)
		}
	}
}

// relayToSettings forwards a settings/<method> request to nvpair-node-settings
// and relays its response straight back. Settings get/set ops are local and
// fast, so unlike the engine relay this uses the bounded synchronous Call
// (the same shape as the proxy relay). The method is forwarded verbatim —
// nvpair-node-settings' methods are already settings/-prefixed.
func (b *Broker) relayToSettings(msg *Message) {
	st := b.getSettings()
	if st == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "node-settings not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}
	result, rpcErr, err := st.Call(context.Background(), msg.Method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("settings call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay settings error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay settings result for %s: %v", msg.Method, err)
		}
	}
}

// relayToManualNodes forwards a manual-node request (node/add, node/remove,
// nodes/list) to nvpair-manual-nodes and relays its response back. node/add
// kicks off an initial probe, so the relay uses the no-timeout async
// RelayRequest rather than the bounded Call. The resulting node/discovered
// notification is merged into the discovery store by
// forwardManualNodesNotification, so the snapshot stays in sync.
func (b *Broker) relayToManualNodes(msg *Message) {
	mn := b.getManualNodes()
	if mn == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "manual-nodes not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}
	id := msg.ID
	method := msg.Method
	relayErr := mn.RelayRequest(method, msg.Params, func(result json.RawMessage, rpcErr *RPCError, err error) {
		switch {
		case err != nil:
			if e := b.codec.RespondError(id, -32000, fmt.Sprintf("manual-nodes call failed: %v", err)); e != nil {
				log.Printf("failed to relay manual-nodes error for %s: %v", method, e)
			}
		case rpcErr != nil:
			if e := b.codec.RespondError(id, rpcErr.Code, rpcErr.Message); e != nil {
				log.Printf("failed to relay manual-nodes error for %s: %v", method, e)
			}
		default:
			if e := b.codec.Respond(id, result); e != nil {
				log.Printf("failed to relay manual-nodes result for %s: %v", method, e)
			}
		}
	})
	if relayErr != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("manual-nodes call failed: %v", relayErr)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	}
}

// relayToClusterManager forwards a cluster:* / nodes:* request to
// nvpair-cluster-manager verbatim and maps its response (result or JSON-RPC
// error) straight back to the client. Cluster get/set + pairing ops are
// local request/response, so it uses the bounded synchronous Call (the same
// shape as the proxy/settings relays). If no cluster-manager is supervised
// the client gets a clear error rather than a silent hang.
func (b *Broker) relayToClusterManager(msg *Message) {
	cm := b.getClusterMgr()
	if cm == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "nvpair-cluster-manager not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}
	result, rpcErr, err := cm.Call(context.Background(), msg.Method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("cluster-manager call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay cluster-manager error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay cluster-manager result for %s: %v", msg.Method, err)
		}
	}
}
