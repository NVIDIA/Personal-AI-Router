// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package noderec is the single source of truth for the consolidated
// _nvpair-node._tcp wire format and the discovery JSON-RPC contract shared by the
// promoted node-scanner daemon, the broker relay, and every consumer of the
// consolidated discovery.
//
// A node advertises itself as ONE _nvpair-node._tcp record
// whose TXT map carries a schema version, the node's identity, its LAN address,
// and one compact key per local service port, e.g.:
//
//	v=1;uuid=<hostUuid>;cluster-uuid=<clusterUuid>;ip=192.168.1.10;ni=14318;ol=11434;lm=1234;er=14319;wl=14320;cl=14321;em=14322
//
// Design decisions this package encodes:
//   - SRV port is a fixed, NON-authoritative constant; consumers ignore it and
//     read every service port from TXT. A missing service key means that
//     service is absent on that node.
//   - Transport is NOT advertised. A consumer derives a service's transport from
//     the static per-service policy here (service type + whether the target node
//     is clustered), because transport is global policy every same-version
//     binary already knows, not per-instance data.
//   - uuid= (hostUuid) and cluster-uuid= (clusterUuid) are distinct fields:
//     invite resolution keys by hostUuid; the cluster pin is selected by
//     clusterUuid, which is present only when the node is clustered.
//   - The model list is NOT in TXT. It moved to HTTP (engine-manager's em
//     /v1/models) because it can grow past the DNS per-string limit; a peer's
//     daemon fetches it during enrichment and puts it on DirectoryNode.Models.
package noderec

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// ServiceType is the single mDNS service type every node advertises.
	ServiceType = "_nvpair-node._tcp"
	// Domain is the mDNS domain.
	Domain = "local"
	// SRVPort is the fixed, non-authoritative SRV port on the _nvpair-node
	// record. Consumers MUST ignore it and read each service's port from TXT;
	// it exists only so the SRV record is well-formed and deterministic.
	SRVPort = 14318
	// SchemaVersion is the current v= TXT schema version. It is for
	// diagnostics/logs/tests and to disambiguate the record format across future
	// wire changes — not for mixed-version compatibility (flag-day rollout).
	SchemaVersion = "1"
)

// TXT keys that are not per-service ports.
const (
	KeySchema      = "v"
	KeyHostUUID    = "uuid"
	KeyClusterUUID = "cluster-uuid"
	KeyIP          = "ip"
	// KeyIPs carries the node's ranked address candidates, IPsSeparator-joined,
	// with KeyIP's value first. A multi-homed node has no single address every
	// peer can reach — a direct-connect link works only for the machine on its
	// far end — so it publishes the order it believes in and lets each consumer
	// confirm by connecting, instead of every consumer re-deriving a guess from
	// the address set.
	KeyIPs = "ips"
)

// IPsSeparator joins the KeyIPs list. A comma cannot appear in an IP literal, so
// it needs no escaping.
const IPsSeparator = ","

// MaxAdvertisedIPs caps the KeyIPs list. Four covers a genuinely multi-homed host
// (LAN, second LAN, and a direct-connect pair) while keeping the entry far inside
// the per-string TXT limit, and it bounds the work a consumer spends walking
// candidates before giving up on a node.
const MaxAdvertisedIPs = 4

// maxTXTStringLen is the DNS per-string limit (RFC 6763 §6.1). Each "key=value"
// TXT entry must fit. With the model list moved to HTTP every key is now short
// and bounded, but the guard stays as defense against a future long key.
const maxTXTStringLen = 255

// ServiceKey is the compact TXT key for one advertised service's port.
type ServiceKey string

const (
	ServiceNodeInfo ServiceKey = "ni"
	ServiceOllama   ServiceKey = "ol"
	ServiceLMStudio ServiceKey = "lm"
	ServiceErrors   ServiceKey = "er"
	ServiceWorkload ServiceKey = "wl"
	ServiceCluster  ServiceKey = "cl"
	// ServiceEngineManager is nvpair-engine-manager's LAN HTTP endpoint (the model
	// list, and future remote engine operations). Its /v1/models is how a peer's
	// discovery daemon learns a node's models now that the list is off the
	// size-limited mDNS TXT record. Plain HTTP (model names aren't secret).
	ServiceEngineManager ServiceKey = "em"
	// ServiceEngineControl is nvpair-engine-manager's cluster-scoped remote-control
	// endpoint (remote install/pull/start/stop + engine status). Unlike em, it
	// is pin-based mTLS (cluster peers only) because it performs privileged
	// operations, and it binds only when the node is clustered.
	ServiceEngineControl ServiceKey = "ec"
)

// serviceKeyOrder is the deterministic emit order for service ports in TXT.
var serviceKeyOrder = []ServiceKey{
	ServiceNodeInfo, ServiceOllama, ServiceLMStudio,
	ServiceErrors, ServiceWorkload, ServiceCluster, ServiceEngineManager,
	ServiceEngineControl,
}

// Transport is the connection policy for a service, derived (not advertised).
type Transport int

const (
	// TransportPlain: always plain HTTP, even on a clustered node (node-info and
	// the local inference engines).
	TransportPlain Transport = iota
	// TransportMTLSWhenClustered: cluster-scoped pin-based mTLS when the target
	// node is clustered, else plain (nvpair-errors, nvpair-workload-manager).
	TransportMTLSWhenClustered
	// TransportSplit: nvpair-cluster-manager's :14321 — a plain PIN-authenticated
	// pairing channel plus mTLS trusted endpoints on one port, demuxed by the
	// first byte. cluster-manager owns this; generic consumers don't dial it.
	TransportSplit
)

// Transport returns the static transport policy for the service.
func (s ServiceKey) Transport() Transport {
	switch s {
	case ServiceErrors, ServiceWorkload, ServiceEngineControl:
		return TransportMTLSWhenClustered
	case ServiceCluster:
		return TransportSplit
	default: // node-info, engines (em model list is plain)
		return TransportPlain
	}
}

// UsesMTLS reports whether a consumer should dial this service over cluster
// mTLS, given whether the target node is clustered. node-info is deliberately
// plain even on a clustered node; errors/workload gate on the target's cluster
// state; the cluster-manager split channel's trusted plane is mTLS.
func (s ServiceKey) UsesMTLS(targetClustered bool) bool {
	switch s.Transport() {
	case TransportPlain:
		return false
	case TransportMTLSWhenClustered, TransportSplit:
		return targetClustered
	default:
		return false
	}
}

// NodeRecord is the parsed _nvpair-node TXT map for one node.
type NodeRecord struct {
	SchemaVersion string
	HostUUID      string
	ClusterUUID   string
	// IP is the node's canonical address: the one it believes most peers can
	// reach, and the one consumers show and try first.
	IP string
	// IPs is the node's full ranked candidate list, IP first. Empty on a record
	// that published only IP, which reads the same as a single-homed node.
	IPs      []string
	Services map[ServiceKey]int
}

// Clustered reports whether the node advertises a cluster identity (its
// cluster-scoped services are mTLS-gated). It is pin-selection state, not a
// membership or trust claim.
func (r NodeRecord) Clustered() bool { return r.ClusterUUID != "" }

// Port returns the advertised port for a service and whether it is present.
func (r NodeRecord) Port(s ServiceKey) (int, bool) {
	p, ok := r.Services[s]
	return p, ok
}

// ParseTXT parses a node record's TXT strings. Unknown keys are ignored;
// malformed service ports are skipped. It never errors — a partial record is
// more useful than none.
func ParseTXT(txt []string) NodeRecord {
	r := NodeRecord{Services: make(map[ServiceKey]int)}
	for _, kv := range txt {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch key {
		case KeySchema:
			r.SchemaVersion = val
		case KeyHostUUID:
			r.HostUUID = val
		case KeyClusterUUID:
			r.ClusterUUID = val
		case KeyIP:
			r.IP = val
		case KeyIPs:
			// Capped on the way in as well as on the way out. The publisher's cap
			// binds only a well-behaved publisher, and a single TXT string has
			// room for roughly sixteen IPv4 literals — every one of which a dialer
			// would walk, at its own per-candidate timeout, before giving up on
			// the node.
			for _, part := range strings.Split(val, IPsSeparator) {
				if len(r.IPs) >= MaxAdvertisedIPs {
					break
				}
				if part = strings.TrimSpace(part); part != "" {
					r.IPs = append(r.IPs, part)
				}
			}
		default:
			if port, err := strconv.Atoi(val); err == nil && port > 0 {
				r.Services[ServiceKey(key)] = port
			}
		}
	}
	return r
}

// TXT builds the record's TXT strings in a deterministic order: schema, identity
// (uuid, then cluster-uuid when set), ip, ips, then service ports in
// serviceKeyOrder. Absent fields are omitted.
//
// ips is emitted only when it says something ip does not — a single-homed node
// publishes one address once rather than twice.
func (r NodeRecord) TXT() []string {
	var txt []string
	ver := r.SchemaVersion
	if ver == "" {
		ver = SchemaVersion
	}
	txt = append(txt, KeySchema+"="+ver)
	if r.HostUUID != "" {
		txt = append(txt, KeyHostUUID+"="+r.HostUUID)
	}
	if r.ClusterUUID != "" {
		txt = append(txt, KeyClusterUUID+"="+r.ClusterUUID)
	}
	if r.IP != "" {
		txt = append(txt, KeyIP+"="+r.IP)
	}
	// Capped here as well as at the publisher: this is the wire boundary, and the
	// per-string TXT limit must hold no matter which caller built the record.
	if ips := r.IPs; len(ips) > 1 {
		if len(ips) > MaxAdvertisedIPs {
			ips = ips[:MaxAdvertisedIPs]
		}
		txt = append(txt, KeyIPs+"="+strings.Join(ips, IPsSeparator))
	}
	for _, s := range serviceKeyOrder {
		if port, ok := r.Services[s]; ok {
			txt = append(txt, string(s)+"="+strconv.Itoa(port))
		}
	}
	// Any service keys not in the known order (forward-compat), sorted. Gated on
	// actual presence of an unknown key, not on the total count: a record can
	// carry an unknown key while holding fewer than len(serviceKeyOrder) services
	// (e.g. 3 known + 1 future), so a count comparison would silently drop it.
	known := make(map[ServiceKey]bool, len(serviceKeyOrder))
	for _, s := range serviceKeyOrder {
		known[s] = true
	}
	var extra []string
	for s, port := range r.Services {
		if !known[s] {
			extra = append(extra, string(s)+"="+strconv.Itoa(port))
		}
	}
	sort.Strings(extra)
	txt = append(txt, extra...)
	return txt
}

// ValidateTXTSize reports the first TXT entry that exceeds the DNS 255-byte
// per-string limit, if any. The daemon logs this as a defensive guard; with the
// model list moved to HTTP no current key can realistically overflow.
func ValidateTXTSize(txt []string) error {
	for _, s := range txt {
		if len(s) > maxTXTStringLen {
			key, _, _ := strings.Cut(s, "=")
			return fmt.Errorf("mDNS TXT entry %q is %d bytes, exceeds the %d-byte limit", key, len(s), maxTXTStringLen)
		}
	}
	return nil
}

// Discovery JSON-RPC contract — the method and notification names the broker
// relay speaks with the daemon and with consumers.
const (
	// Worker -> broker (relayed to the daemon). Register is an id-bearing
	// request the broker acks; the others are notifications.
	MethodRegister   = "discovery:register"
	MethodUnregister = "discovery:unregister"
	MethodUpdateTXT  = "discovery:update-txt"
	// MethodReloadIdentity tells the daemon to re-resolve this node's advertised
	// identity (hostUuid/clusterUuid/ip) and re-advertise if it changed. The
	// broker triggers it once cluster-manager is up, so the scanner's uuid=
	// converges on the cluster principal even though the scanner spawned (and
	// minted its own node-id) before cluster-manager wrote identity.json.
	MethodReloadIdentity = "discovery:reload-identity"
	// MethodReloadTrust tells the daemon that this node's cluster pin set changed,
	// so it must re-derive the trusted annotation on every directory entry. The
	// daemon otherwise answers "do I hold a pin for this peer?" only when that
	// peer's mDNS record moves, and a peer already advertising when we join never
	// moves again — leaving it annotated from before our pins existed. The broker
	// triggers this off nvpair-cluster-manager, which announces only after the pin
	// is on disk, so the daemon always reads a directory at least as new as the
	// event.
	MethodReloadTrust = "discovery:reload-trust"

	// MethodSetClusterIdentity tells nvpair-node-info the cluster principal this
	// node currently holds, so it can report it on /v1/node-info.
	//
	// node-info is spawned with no cluster dir on purpose — it is the one
	// inter-node surface kept plain so any LAN peer can read this host's
	// inventory (see the broker's spawnNodeInfo) — which also leaves it unable to
	// read membership for itself. The broker owns that fact and pushes it here on
	// startup and on every change, so node-info reports one live value rather
	// than deriving a second one.
	//
	// The value is a peer's only mDNS-independent way to learn that this node has
	// left a cluster: cluster membership otherwise reaches the fleet solely as the
	// cluster-uuid= TXT key, and a consumer that stops receiving this node's mDNS
	// record keeps its last observed value indefinitely.
	MethodSetClusterIdentity = "nodeinfo:set-cluster-identity"

	// NotifyObservedAddresses is nvpair-node-info -> broker: the local addresses
	// peers have actually reached this node on, learned from its own accepted
	// connections.
	//
	// This is the only direct evidence a host can have about which of its
	// addresses work from somewhere else. Everything else it can see — routes,
	// interface flags, multicast success — describes the link from this side. A
	// peer completing a request proves the whole path, and the desktop polls every
	// peer's node-info continuously, so the proof arrives on its own.
	NotifyObservedAddresses = "nodeinfo:observed-addresses"

	// MethodSetObservedAddresses is broker -> nvpair-node-scanner, relaying
	// NotifyObservedAddresses so address selection can rank a peer-proven address
	// above one it merely inferred.
	MethodSetObservedAddresses = "discovery:set-observed-addresses"

	// NotifyNodeActivity is ollama-proxy / lmstudio-proxy -> broker: a peer's
	// engine just returned inference response bytes to us.
	//
	// It is a liveness report, not a metric. A peer that is streaming a
	// generation back has proved its network path, its OS, and its engine process
	// are all working, which no control-plane probe can establish more cheaply —
	// and unlike a probe, the evidence arrives most reliably when the peer is
	// busiest, which is when its probes fail.
	NotifyNodeActivity = "node/activity"

	// MethodNodeActivity is broker -> nvpair-node-scanner, relaying
	// NotifyNodeActivity so the eviction path can prefer proof that a node is
	// working over the silence of a timed-out probe.
	MethodNodeActivity = "discovery:node-activity"

	// Consumer -> broker.
	MethodGetNodes  = "discovery:get-nodes"
	MethodSubscribe = "discovery:subscribe"

	// Daemon -> broker push notifications: the node-scanner's mDNS browse is
	// naturally event-based, so it emits these per-node deltas, which the broker
	// relay folds into its full directory.
	NotifyNodeDiscovered = "discovery:node-discovered"
	NotifyNodeUpdated    = "discovery:node-updated"
	NotifyNodeRemoved    = "discovery:node-removed"
	NotifyNodeTelemetry  = "discovery:node-telemetry"

	// NotifyNodes is the broker relay -> subscriber push: the subscriber's full
	// filtered node set, re-sent on every change. Consumers replace their set
	// from it (stateless + self-correcting) rather than applying per-node deltas,
	// matching how the client-facing discovery:nodes-changed already works.
	NotifyNodes = "discovery:nodes"
)

// RegisterParams is the canonical discovery:register / update-txt payload: a
// service advertises its own key, port, and any TXT it contributes. Transport is
// deliberately NOT carried — it is 100% policy-derived (see ServiceKey.Transport).
type RegisterParams struct {
	Service ServiceKey `json:"service"`
	Port    int        `json:"port"`
	TXT     []string   `json:"txt,omitempty"`
}

// UnregisterParams removes a previously registered service.
type UnregisterParams struct {
	Service ServiceKey `json:"service"`
}

// ClusterIdentityParams carries the cluster principal for
// MethodSetClusterIdentity. Empty means this node belongs to no cluster, which is
// a meaningful value rather than an absent one — it is how a departure is
// announced — so the field is always sent.
type ClusterIdentityParams struct {
	ClusterUUID string `json:"clusterUuid"`
}

// ObservedAddressesParams carries the local addresses remote peers have reached
// this node on, for NotifyObservedAddresses and MethodSetObservedAddresses. The
// set is always complete: a receiver replaces what it holds, so an address a peer
// has stopped using stops counting as proof.
type ObservedAddressesParams struct {
	Addresses []string `json:"addresses"`
}

// NodeActivityParams reports that a peer's engine returned inference response
// bytes MSSince milliseconds ago, for NotifyNodeActivity and MethodNodeActivity.
//
// The age is carried rather than a timestamp for the same reason NodeTelemetry
// does it: producer and consumer are separate processes, and an elapsed
// milliseconds figure needs no agreement about clocks.
type NodeActivityParams struct {
	HostUUID string `json:"hostUuid"`
	MSSince  int64  `json:"msSince"`
}

// ReadyParams is the daemon's startup notification. Epoch increments each time
// the daemon (re)starts; the broker relay replays all registrations and
// subscriptions when it sees a new epoch, so no worker needs reconnect logic.
type ReadyParams struct {
	Version string `json:"version"`
	Epoch   int64  `json:"epoch,omitempty"`
}

// GPUInfo / CPUInfo / MemoryInfo are the node-info enrichment carried on a
// directory entry. Consolidated here so the daemon, the broker relay, and
// clients share one shape.
type GPUInfo struct {
	Name               string `json:"name"`
	VramBytes          uint64 `json:"vram_bytes,omitempty"`
	VramUsedBytes      uint64 `json:"vram_used_bytes,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`
}

type CPUInfo struct {
	Name               string `json:"name,omitempty"`
	Cores              uint32 `json:"cores,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`
}

type MemoryInfo struct {
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	UsedBytes  uint64 `json:"used_bytes,omitempty"`
}

// ProbeStatus is a service's per-service liveness facet. Liveness is kept
// distinct from trust: an untrusted gated service (a 403/TLS failure against a
// peer we hold no pin for) is "inaccessible", never "unreachable" — only a
// service that is present, probeable by us, and failing is "unreachable".
type ProbeStatus string

const (
	ProbeUnknown      ProbeStatus = ""             // not probed (e.g. a local service, or not yet)
	ProbeReachable    ProbeStatus = "reachable"    // TCP/health OK
	ProbeUnreachable  ProbeStatus = "unreachable"  // present + probeable-by-us + failing
	ProbeInaccessible ProbeStatus = "inaccessible" // present but gated/untrusted to us
)

// ServiceStatus is a directory entry's per-service record: the advertised port
// plus its liveness facet.
//
// Probe is reserved and currently always ProbeUnknown: the daemon does not yet
// report a per-service liveness facet to subscribers. The critical liveness
// guarantee — the TCP-probe-before-evict anti-flap guard — IS
// implemented on the daemon (it keeps a still-reachable node across a transient
// mDNS miss); surfacing per-service reachable/unreachable/inaccessible here is a
// deferred reporting feature (it would add a probe per service per browse).
type ServiceStatus struct {
	Port  int         `json:"port"`
	Probe ProbeStatus `json:"probe,omitempty"`
}

// DirectoryNode is one node in the consolidated directory served by
// discovery:get-nodes, carried in the daemon's discovery:node-* events, and
// listed in the broker's discovery:nodes snapshots. It replaces
// the per-service EnrichedNode: identity is the stable hostUuid, transport is
// derived from Services + Clustered (never carried), and node-info enrichment
// decorates entries that advertise a node-info service.
type DirectoryNode struct {
	HostUUID string `json:"hostUuid"`
	Name     string `json:"name"` // mDNS instance name (hostname)
	IP       string `json:"ip"`   // canonical dialable LAN address
	// IPs is every address this node published, in its own ranked order with IP
	// first. A multi-homed node has no single address every peer can reach — a
	// direct-connect link is reachable only from the machine on its far end — so
	// a consumer that must connect walks this list instead of treating IP as the
	// only answer. Empty for a node that published one address.
	IPs         []string                     `json:"ips,omitempty"`
	ClusterUUID string                       `json:"clusterUuid,omitempty"`
	Trusted     bool                         `json:"trusted"` // we hold a cluster pin for it
	Services    map[ServiceKey]ServiceStatus `json:"services"`
	GPUs        []GPUInfo                    `json:"gpus,omitempty"`
	CPU         *CPUInfo                     `json:"cpu,omitempty"`
	Memory      *MemoryInfo                  `json:"memory,omitempty"`
	Models      []string                     `json:"models,omitempty"` // enriched from engine-manager's em /v1/models
	// ModelsByEngine attributes the enriched model list to the engine that
	// serves each model, keyed by engine-manager engine name (e.g. "ollama",
	// "lmstudio"). Models is the flat de-duplicated union of these lists and is
	// retained unchanged for consumers that only need the node-level set (e.g.
	// the proxies' model-owner routing); ModelsByEngine is the additive, opt-in
	// attribution a per-engine consumer (the UI's engine card) needs. Enriched
	// from engine-manager's em /v1/models modelsByEngine field. A present engine
	// key with [] means its inventory was successfully queried and is empty; a
	// missing key means it was not running/queryable. Omitted when no engine
	// inventory was successfully reported.
	ModelsByEngine map[string][]string `json:"modelsByEngine,omitempty"`
	// LoadedByEngine names the models currently resident in memory on this node,
	// per engine (normally a subset of ModelsByEngine). Enriched from
	// engine-manager's em /v1/models loadedByEngine field so a remote node's
	// model cards can reflect loaded state too. An engine key with an
	// empty list means "running, nothing loaded"; a missing key means the peer
	// didn't report loaded state for it. Omitted when the peer reports none.
	LoadedByEngine map[string][]string `json:"loadedByEngine,omitempty"`
	// LoadedVRAMByEngine reports last-observed GPU-resident bytes per engine/model.
	// It is not a minimum requirement or an admission signal. Zero is known; absent is unknown.
	LoadedVRAMByEngine map[string]map[string]uint64 `json:"loadedVramByEngine,omitempty"`
	LastSeen           int64                        `json:"lastSeen"` // Unix seconds
}

// Clustered reports whether the node advertises a cluster identity.
func (n DirectoryNode) Clustered() bool { return n.ClusterUUID != "" }

// CandidateIPs returns the node's addresses in its own ranked order, canonical
// first. It reads a record that published only IP as the single-address list it
// describes, so consumers never branch on which field a node filled in.
func (n DirectoryNode) CandidateIPs() []string {
	if len(n.IPs) > 0 {
		return n.IPs
	}
	if n.IP != "" {
		return []string{n.IP}
	}
	return nil
}

// AddressTXT returns the ip= (and, for a multi-homed node, ips=) TXT strings for
// this node's published addresses.
//
// It exists so every consumer that re-projects a directory node onto a TXT-shaped
// value — the broker's enriched node, both inference proxies, the cluster
// resolver — emits the same keys in the same order. Each of them built this by
// hand before, and each of them collapsed the address list to one entry while
// doing it, which is what left a caller with a single unreachable address and
// nothing else to try.
func (n DirectoryNode) AddressTXT() []string {
	ips := n.CandidateIPs()
	if len(ips) == 0 {
		return nil
	}
	txt := []string{KeyIP + "=" + ips[0]}
	if len(ips) > 1 {
		txt = append(txt, KeyIPs+"="+strings.Join(ips, IPsSeparator))
	}
	return txt
}

// HasService reports whether the node advertises the given service.
func (n DirectoryNode) HasService(s ServiceKey) bool {
	_, ok := n.Services[s]
	return ok
}

// EngineModels returns the models a single engine serves on this node, by
// engine-manager engine name (e.g. "ollama", "lmstudio"). It prefers the
// per-engine attribution in ModelsByEngine so a consumer that reasons about one
// engine (a proxy ranking model owners for its own engine, or the UI's engine
// card) never conflates another engine's models. When the node carries
// attribution but no entry for this engine, that engine authoritatively serves
// nothing and the result is empty — NOT the cross-engine union. Only when the
// node carries no attribution at all (ModelsByEngine nil — a pre-attribution or
// mixed-version peer) does it fall back to the flat Models union, so a consumer
// never regresses to "no inventory" against an older peer.
func (n DirectoryNode) EngineModels(engine string) []string {
	if n.ModelsByEngine != nil {
		return n.ModelsByEngine[engine]
	}
	return n.Models
}

// SubscribeParams filters a subscription to nodes advertising any of the listed
// services; an empty list subscribes to all nodes.
type SubscribeParams struct {
	Services []ServiceKey `json:"services,omitempty"`
}

// Matches reports whether a node passes the subscription filter.
func (p SubscribeParams) Matches(n DirectoryNode) bool {
	if len(p.Services) == 0 {
		return true
	}
	for _, s := range p.Services {
		if n.HasService(s) {
			return true
		}
	}
	return false
}

// GetNodesParams optionally filters discovery:get-nodes to a single service.
type GetNodesParams struct {
	Service ServiceKey `json:"service,omitempty"`
}

// GetNodesResult is the discovery:get-nodes response.
type GetNodesResult struct {
	Nodes []DirectoryNode `json:"nodes"`
}

// NodeEvent is the payload of a discovery:node-discovered/updated/removed
// notification: the affected directory node (for removed, identity + last-known
// fields).
type NodeEvent struct {
	Node DirectoryNode `json:"node"`
}

// NodeTelemetry is a fresh GPU-utilization observation for one node. The
// scanner emits it independently of directory changes so scheduling telemetry
// stays current even when discovery identity and inventory are stable.
type NodeTelemetry struct {
	HostUUID          string `json:"hostUuid"`
	GPUUtilizationPct uint32 `json:"gpuUtilizationPercent"`
	TelemetryValid    bool   `json:"telemetryValid"`
	MSSince           int64  `json:"msSince"`
}
