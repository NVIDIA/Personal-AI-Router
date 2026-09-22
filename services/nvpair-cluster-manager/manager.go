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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
	"nvpair-shared/reach"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// defaultPort is the inter-node HTTP listener port (one above
// nvpair-workload-manager's 14320).
const defaultPort = 14321

// Manager owns the cluster-manager's local JSON-RPC surface. Later commits add
// the node identity, trusted-node store, membership set, and the inter-node
// HTTP/mTLS + mDNS machinery as fields here.
type Manager struct {
	codec      *Codec
	clusterDir string
	port       int

	identity *NodeIdentity
	trust    *TrustStore
	// mesh supplies the pool's reloadable local identity; TrustStore is the pin
	// authority for both inbound and outbound cluster-manager traffic. clients
	// is the long-lived per-peer mTLS pool — a throwaway Transport per reconcile
	// leaked a socket for the life of the process.
	mesh    *clustertrust.Mesh
	clients *clustertrust.PeerClientPool

	clusterMu            sync.RWMutex
	clusterID            string
	clusterFriendlyName  string
	inviteCreatedCluster bool   // true when founded solely to back invite pairing
	inviteCreatedID      string // cluster id recorded in the durable provenance marker

	// admissionMu guards the durable node-global admission counter and this
	// node's current cluster incarnation. Unlike clusterGen (an in-process
	// compare-and-commit guard), admissionEpoch survives restart and is carried
	// in signed membership/removal evidence so an old removal cannot evict a
	// legitimate same-cluster readmission.
	admissionMu        sync.Mutex
	admissionCounter   uint64
	admissionActivated uint64
	admissionRetired   bool
	admissionClusterID string
	admissionEpoch     uint64

	// inviteMu serializes outbound invite registration, terminal transitions,
	// Completion commits, and invite-created cluster cleanup. It prevents one
	// invite from tearing down the cluster while a sibling is being registered
	// or committing.
	inviteMu sync.Mutex

	// clusterGen is bumped on every change to this node's local cluster
	// composition — identity adopt/create/teardown and membership add/remove.
	// The periodic self-remove guard (reconcilePeersAndMaybeSelfRemove)
	// snapshots it before its network fan-out and rechecks it under rosterMu
	// before tearing down, so a pairing/rejoin that lands during the reconcile
	// wait is never erased by a now-stale unanimous-403 verdict.
	clusterGen atomic.Uint64

	memMu   sync.Mutex
	members map[string]*ClusterNode // keyed by nodeUuid
	invites map[string]*Invite      // keyed by inviteId

	tsMu       sync.Mutex
	tombstones map[string]Tombstone // keyed by removed nodeUuid; newest wins (fan-out removal)

	proofMu       sync.Mutex
	removalProofs map[string]RemovalProof // keyed by removed nodeUuid; highest admission epoch wins

	// rosterMu is the cluster-composition boundary. It serializes every change to
	// this node's identity/trust/membership — mergeRoster, and (via
	// withClusterComposition) adopt-on-join, cluster:create, cluster:set-identity,
	// and the inbound pairing commit — against teardownClusterLocal and the
	// periodic self-remove compare-and-teardown. Holding it for the whole of each
	// commit makes teardown linearizable against them, so a rejoin/adopt can never
	// interleave with an in-flight self-eviction (leaving pins/members half-torn-
	// down) nor be erased by a now-stale unanimous-403 verdict.
	rosterMu sync.Mutex

	// clusterTornDown latches once a teardown (cluster:leave, an inbound peer
	// removal, or the offline self-remove) has cleared this node's cluster in
	// this process. It gates cluster:set-identity: a nonempty-id restore is only
	// honored before a teardown (a clean startup restore) or as a no-op re-assert
	// of the id we already hold — never to resurrect a cluster we deliberately
	// left with an empty roster. Re-clustering after a teardown goes through
	// pairing/create (which set the identity directly), not set-identity. Guarded
	// by rosterMu (set in teardownClusterLocalLocked, read in handleSetIdentity's
	// composition closure).
	clusterTornDown bool
	// teardownPending is true from the durable journal write until every
	// authorization surface and the marker are cleared. While set, no endpoint
	// may authenticate an old pin or establish a new cluster admission.
	teardownPending atomic.Bool

	// sessGen counts how many times resetSessions has cleared the pairing
	// session map — i.e. how many teardowns (leave / inbound removal / offline
	// self-remove) have abandoned in-flight pairings in this process.
	// handleInviteNode snapshots it when it begins an Initial Exchange, and
	// registerInviterSession refuses to install the session if it has changed
	// since — so a teardown that raced the exchange abandons the pairing even if
	// this node rejoined the same cluster (a rejoin does not clear sessions).
	// Only resetSessions bumps it, so an unrelated membership change or a sibling
	// invite never trips it.
	sessGen  atomic.Uint64
	sessMu   sync.Mutex
	sessions map[string]*pairingSession // keyed by inviteId

	browser *Browser

	// observedPeerHosts maps a member's uuid to the source host it was last seen
	// connecting from over an authenticated transport. Memory-only: it is
	// re-earned by the first inbound reconcile after a restart, and a stale
	// persisted copy would be a claim rather than an observation.
	observedMu        sync.Mutex
	observedPeerHosts map[string]string

	// peerAddrs remembers which of a member's advertised addresses answers, so a
	// multi-homed peer is reconciled with at an address that works rather than at
	// whichever one it happens to rank first.
	peerAddrs *reach.Chooser

	cancel context.CancelFunc

	// Test-only removal boundary hooks; nil in production.
	testRemovalPrepared chan struct{}
	testRemovalContinue chan struct{}
}

// clusterIdentity returns the current cluster id and friendly name. An empty
// clusterId means this node is unclustered.
func (m *Manager) clusterIdentity() (id, friendly string) {
	m.clusterMu.RLock()
	defer m.clusterMu.RUnlock()
	return m.clusterID, m.clusterFriendlyName
}

func (m *Manager) setClusterIdentity(id, friendly string) {
	m.clusterMu.Lock()
	changed := m.clusterID != id || m.clusterFriendlyName != friendly
	m.clusterID = id
	m.clusterFriendlyName = friendly
	m.clusterMu.Unlock()
	// Adopting, founding, or clearing the cluster identity changes our
	// composition; bump so an in-flight self-remove verdict is invalidated.
	// Only a real change counts: a no-op re-assert (e.g. the Broker re-syncing
	// the same clusterId from node-settings) must not bump the generation, or it
	// would spuriously invalidate a legitimate unanimous-403 verdict landing in
	// the same reconcile pass and postpone the offline-removed self-remove.
	if changed {
		m.clusterGen.Add(1)
	}
}

// withClusterComposition runs a cluster-composition change — identity
// adopt/create/set plus the pinning and membership records that go with a
// pairing — while holding rosterMu, the same boundary that guards
// teardownClusterLocal and the periodic self-remove compare-and-teardown.
// Holding it for the entire commit makes the commit atomic with respect to a
// concurrent teardown: a racing self-eviction either observes the commit and
// goes stale, or runs a full teardown that the commit then cleanly re-
// establishes on top of — never a partial interleave. fn MUST NOT call anything
// that re-acquires rosterMu (mergeRoster, teardownClusterLocal).
func (m *Manager) withClusterComposition(fn func()) {
	m.rosterMu.Lock()
	defer m.rosterMu.Unlock()
	fn()
}

// emitIdentityChanged notifies the Broker that the local cluster identity
// originated here (cluster:create or adopt-on-join) so it can persist it to
// nvpair-node-settings.
func (m *Manager) emitIdentityChanged() {
	id, friendly := m.clusterIdentity()
	if err := m.codec.Notify("cluster:identity-changed", map[string]any{
		"clusterId":           id,
		"clusterFriendlyName": friendly,
	}); err != nil {
		log.Printf("emit cluster:identity-changed: %v", err)
	}
}

// NewManager constructs a Manager. configDir overrides the base config
// directory; the durable cluster subtree lives at <base>/cluster.
func NewManager(codec *Codec, configDir string, port int) (*Manager, error) {
	base := configDir
	if base == "" {
		base = defaultBaseDir()
	}
	clusterDir := filepath.Join(base, "cluster")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cluster dir %s: %w", clusterDir, err)
	}
	identity, err := loadOrMintIdentity(clusterDir)
	if err != nil {
		return nil, fmt.Errorf("load node identity: %w", err)
	}
	log.Printf("node identity %s (host %s) fingerprint %s", identity.NodeUUID, identity.NodeID, identity.CertFingerprint)
	trust, err := newTrustStore(clusterDir)
	if err != nil {
		return nil, fmt.Errorf("open trusted store: %w", err)
	}
	mesh := clustertrust.Open(clusterDir)
	mgr := &Manager{
		codec:      codec,
		clusterDir: clusterDir,
		port:       port,
		identity:   identity,
		trust:      trust,
		mesh:       mesh,
		clients: clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{
			Timeout:    pairingHTTPTimeout,
			ResolvePin: trust.DER,
		}),
		members:       make(map[string]*ClusterNode),
		invites:       make(map[string]*Invite),
		tombstones:    make(map[string]Tombstone),
		removalProofs: make(map[string]RemovalProof),
		sessions:      make(map[string]*pairingSession),
		peerAddrs:     reach.NewChooser(),
	}
	// Announce every pin-set mutation. This node's other workers cache answers
	// derived from the trusted/ directory — above all "may work be routed to this
	// peer?" — and this process is the only writer, so without this they can only
	// learn of a pairing or a removal by re-reading on a timer. Registered here so
	// it covers the startup prune below as well as everything at runtime.
	trust.SetOnChange(mgr.announceTrustChanged)
	if err := mgr.loadAdmission(); err != nil {
		return nil, fmt.Errorf("load admission state: %w", err)
	}
	if cid, epoch := mgr.currentAdmission(); cid != "" && epoch != 0 {
		// admission.json is the crash-consistent local authority. Restore it
		// before the broker reflects node-settings so a crash after pairing but
		// before settings persistence does not forget a committed cluster.
		mgr.clusterID = cid
	}
	if mgr.admissionWasRetired() {
		// Persist the stale-settings gate across process restarts.
		mgr.clusterTornDown = true
	}
	if err := mgr.loadMembers(); err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	if err := mgr.rollbackIncompleteAdmission(); err != nil {
		return nil, fmt.Errorf("rollback incomplete admission: %w", err)
	}
	if cid, epoch := mgr.currentAdmission(); cid != "" && epoch != 0 {
		if err := mgr.migrateLegacyAdmissions(cid, epoch); err != nil {
			return nil, fmt.Errorf("migrate legacy admissions: %w", err)
		}
		if err := mgr.pruneUnauthenticatedMembers(cid, epoch); err != nil {
			return nil, fmt.Errorf("prune unauthenticated members: %w", err)
		}
	}
	// A PC rename changes os.Hostname() but not the stable nodeUuid, so the self
	// entry restored from members.json can carry an old display name. Re-stamp it
	// from the current identity so this node doesn't linger as a stale-named ghost
	// in its own roster, preserving stable identity across host renames.
	mgr.refreshSelfMemberIdentity()
	mgr.loadTombstones()
	if err := mgr.loadRemovalProofs(); err != nil {
		return nil, fmt.Errorf("load removal proofs: %w", err)
	}
	if err := mgr.loadInviteCreatedCluster(); err != nil {
		return nil, fmt.Errorf("load invite-created cluster provenance: %w", err)
	}
	if err := mgr.recoverPendingTeardown(); err != nil {
		return nil, fmt.Errorf("recover pending teardown: %w", err)
	}
	if err := mgr.replayRemovalProofs(); err != nil {
		return nil, fmt.Errorf("replay removal proofs: %w", err)
	}
	return mgr, nil
}

// defaultBaseDir resolves the per-user config directory, mirroring the other
// subprocesses, with a next-to-exe fallback for dev builds.
func defaultBaseDir() string {
	if dir, err := appdir.Dir(); err == nil {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// Run starts the read loop and blocks until the context is cancelled or the
// local transport reaches EOF (the parent closed the pipe).
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()

	go func() {
		if err := m.runHTTP(ctx); err != nil {
			log.Printf("inter-node HTTP server error: %v", err)
			cancel()
		}
	}()

	m.startResolver()

	// Subscribe to the broker's discovery relay for cl peer targets. They arrive
	// as discovery:nodes snapshots (handled in handleMessage), each replacing the
	// browser's relay overlay, backing invite/member address resolution.
	// Non-fatal: if the parent isn't a relay-aware broker, only Broker-supplied
	// addresses resolve peers (there is no self-run mDNS fallback post-cutover).
	if err := m.codec.Notify(noderec.MethodSubscribe, noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceCluster}}); err != nil {
		log.Printf("failed to subscribe to discovery relay: %v", err)
	}

	go m.reconcileLoop(ctx)
	go m.inviteExpiryLoop(ctx)

	err := m.readLoop(ctx)
	m.clients.CloseIdle()
	return err
}

// startResolver sets up the peer-address resolver. Post-cutover there is no
// per-service mDNS responder/browse; it degrades gracefully to Broker-supplied
// addresses (§7.5) and the relay-fed overlay.
func (m *Manager) startResolver() {
	// Post-cutover: no per-service _nvpair-cluster-manager advertise/browse.
	// This node's cl=<port> is carried in the node-scanner daemon's single
	// _nvpair-node record, and the peer resolver is fed by the broker's discovery
	// relay via setRelay. A Broker-supplied address still always wins; Resolve
	// is the fallback.
	m.browser = newBrowser()
}

// applyDiscovery replaces the browser's resolver map from a discovery:nodes
// snapshot pushed down by the broker relay, which backs invite/member address
// resolution.
func (m *Manager) applyDiscovery(msg *Message) {
	if m.browser == nil {
		return
	}
	var res noderec.GetNodesResult
	if err := json.Unmarshal(msg.Params, &res); err != nil {
		log.Printf("invalid discovery:nodes snapshot: %v", err)
		return
	}
	m.browser.setRelay(res.Nodes)
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			var de *DecodeError
			if stderrors.As(err, &de) {
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
				m.codec.RespondError(msg.ID, codeInvalidParams, err.Error())
				return
			}
			m.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		return
	}

	if msg.IsNotification() {
		switch msg.Method {
		case noderec.NotifyNodes:
			m.applyDiscovery(msg)
		default:
			log.Printf("ignoring unhandled notification %q", msg.Method)
		}
		return
	}
	if !msg.IsRequest() {
		return
	}

	switch msg.Method {
	case "cluster:get-node-id":
		m.handleGetNodeID(msg)
	case "cluster:set-identity":
		m.handleSetIdentity(msg)
	case "cluster:create":
		m.handleCreate(msg)
	case "nodes:get-initial":
		m.handleGetInitial(msg)
	case "cluster:invite-node":
		m.handleInviteNode(msg)
	case "cluster:respond-to-invite":
		m.handleRespondToInvite(msg)
	case "cluster:cancel-invite":
		m.handleCancelInvite(msg)
	case "cluster:invite-status":
		m.handleInviteStatus(msg)
	case "nodes:remove":
		m.handleNodesRemove(msg)
	case "cluster:leave":
		m.handleLeave(msg)
	case "shutdown":
		m.codec.Respond(msg.ID, nil)
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()
	default:
		m.codec.RespondError(msg.ID, codeMethodNotFound, fmt.Sprintf("method not found: %s", msg.Method))
	}
}
