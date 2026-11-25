// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The cluster router is a second routing personality, not a second proxy. It
// shares this process's transports, its cluster trust, and its workload
// stream; what it adds is an addressing class.
//
// A dedicated facade answers one engine's dialect and can only route to owners
// of that engine. The router answers the OpenAI-compatible surface and routes
// to whoever provides the named model — any (node, engine) pair — after
// rewriting the request body's "model" to the identifier the chosen engine
// expects. That is the whole feature: a client configures one endpoint and one
// name per model instead of one endpoint and one exact name per engine.
//
// Two properties keep it from becoming an amplification hazard, and both are
// load-bearing:
//
//  1. Every hop is terminal. A peer's advertised engine endpoint is that
//     peer's proxy, and a request arriving there over the cluster ingress is
//     forwarded straight to that peer's local engine without consulting
//     discovery. Nothing re-routes a second time, so a cluster-routed request
//     is dialled exactly once and a request loop between two routers is not a
//     state this design can reach. The self-forward guard below applies the
//     same rule on this node.
//  2. The provider set is the boundary. Candidates come from resolving the
//     requested name, and failover walks that set. When it is empty the
//     request is answered locally with a 502 and nothing is dialled — "I
//     cannot reach anybody who serves this" never becomes "let me try
//     whatever is up".
//
// Scope deliberately left out of this prototype, each with its reason:
//
//   - Capacity. The router takes its claim from the same process-wide ledger the
//     dedicated facades use, keyed by the same node ids, and it records the same
//     first-byte evidence: a burst through the router and a burst through a
//     facade see each other's load and each other's stalled providers. What is
//     open is the key's granularity, not the claim. Today's host-level claim
//     (one GPU, whatever engine is holding it) stands; any (host, engine)
//     split is deferred until a mixed-host burst run shows starvation. This file
//     therefore does
//     not invent a second answer to that question; it joins the existing ledger.
//   - The dropped-target watcher. The facades poll a dispatched node's discovery
//     record during an attempt and abandon early when it disappears. This path
//     finds out when the dispatch fails or the first-byte budget expires, so the
//     bound is failover latency, not correctness: the hop still counts once
//     against the provider set, and nothing re-routes.
//   - The paired-addressed telemetry pair (request-started/request). It
//     travels through a facade because the broker attributes those events to
//     an engine; a cluster-scoped stream needs a broker-side decision about
//     what engine "cluster" means there. Workload events below stay on the
//     process-scoped channel, which already accepts any engine tag.
//   - The LAN surface. The listener binds loopback only, so there is no
//     plaintext cluster-wide relay exposed to a network. A clustered mTLS
//     personality for the router is deferred, not declined.
//   - Control-plane fan-out (/api/tags, /api/show, /api/pull and friends).
//     The router is inference plus a generated inventory; it never fetches a
//     list from upstream, so the credential-stripping question the model-list
//     fan-out carries does not arise here. Inference forwards the client's
//     Authorization through, exactly as the dedicated facades do.
//   - Per-engine user pins (facade.SelectedID). A cluster route has no engine to
//     inherit a pin from, so nothing is exempt from load; the ledger still counts
//     the request. Re-resolution between dispatch attempts is also absent: this is
//     one pass over the provider set the resolver produced, and the number of
//     dispatches in it is capped by maxDispatchAttempts. When that cap ends the
//     walk with providers untried the client gets a local 503, not the last
//     provider's 503 read back as the cluster's verdict.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/noderec"
)

// routerDefaultPort is the cluster endpoint's port — what a router/set-enabled
// payload that names no port gets. It is deliberately outside the range every
// engine profile owns, for the same reason the facades' StandalonePort values
// are: a router sitting on an engine's port would be mistaken for that engine,
// both by clients and by engine-manager's identity probe.
//
// It is a var, not a const, for the same reason firstBodyTimeout is: a test
// retunes it and asserts the default-port path against a port the test knows
// is free, instead of binding 9999 off whoever else runs it.
var routerDefaultPort = 9999

// routerClusterEngine is the Engine field of every workload the router
// reports, and the owned_by of every row of its generated inventory.
//
// A workload's identity is (originatedFrom, engine, runId, id), so naming the
// routing class here is what keeps the router's job ids separate from both
// engines' without inventing a fifth component: "cluster" is not an engine id
// in nvpair-shared/engines, and no facade will ever stamp it. It also says
// something true — at the moment the job was submitted, which engine would
// run it was not yet known.
const routerClusterEngine = "cluster"

// routerCatalogTTL bounds how stale the routing catalog may get between
// rebuilds.
//
// Rebuilding reads the per-engine routing overlays this process already keeps,
// so the cost is one pass over a handful of nodes and their inventories; the
// TTL exists to stop a burst of requests from each paying it. A rebuild
// replaces the whole index, so a routing decision sees either the old cluster
// or the new one — the same replace-whole shape the discovery overlay uses.
const routerCatalogTTL = 250 * time.Millisecond

// routerHop is one dial-ready provider: the candidate to forward to, the
// engine it runs, the native model name to rewrite into the body, and the
// facade whose address chooser selected the candidate. The facade travels with
// the hop because a transport error has to forget the address on the same
// chooser that confirmed it.
type routerHop struct {
	cand   candidate
	engine string
	native string
	f      *facade
}

// clusterRouter is the cluster endpoint: one listener, one catalog, and the
// request path that reads it.
type clusterRouter struct {
	// host is the process hosting this router, reached for the shared
	// transports, trust fabric, scheduler baseline and workload stream.
	host *Proxy

	port int
	srv  *http.Server
	ln   net.Listener

	// stopped marks the endpoint as closed — by the runtime turn-off, by
	// teardown, or by the serve context unwinding, whichever reaches it
	// first (they may all reach the same listener). It is what keeps
	// router/status honest: a closed net.Listener still reports its
	// address, so "the listener exists" is not the question a status read
	// answers.
	stopped atomic.Bool

	// mu guards the catalog cache, the configured alias list, and the
	// relay-fed peer alias feed. Rebuilds happen on the request path, which is
	// why the TTL exists: without it every request would serialize on a
	// rebuild.
	mu        sync.Mutex
	catalog   *routerCatalog
	catalogAt time.Time
	// aliases is the configured cluster-wide synonym list, replaced whole by
	// setAliases. Each entry is two names — what a client may call a model and
	// which model that is — never a host list, so the same list routes on every
	// computer that hears it. A name is only ever a synonym for something some
	// engine already serves, so starting from an empty set cannot hide a
	// routable model: resolution finds native identifiers without it.
	aliases []noderec.ModelAlias

	// peerAliasNodes is the relay-fed alias feed: the cluster directory's
	// records for pinned and trusted peer computers, replaced wholesale by
	// every cluster:peer-aliases snapshot. This is where a synonym written on
	// another machine arrives — deliberately not through the per-engine
	// routing overlays, because an alias names a model, not the machines
	// fronting it, and a node that advertises no engine service announces
	// aliases just as validly as one that does.
	//
	// This computer's own record is filtered out: its aliases are
	// configuration, and they reach the router through setAliases, which is
	// immediate rather than discovery-paced.
	peerAliasNodes []noderec.DirectoryNode

	// nextRequestID is the router's own job-id counter, starting at 1.
	//
	// Per personality, not shared with the facades: the workload store's key
	// carries the engine, so a router job and an Ollama-facade job may both
	// be id "1" and remain distinct records.
	nextRequestID atomic.Uint64
}

func newClusterRouter(host *Proxy, port int) *clusterRouter {
	return &clusterRouter{host: host, port: port}
}

// errRouterBindFailed marks a bring-up refused because the requested port
// could not be listened on. The control plane answers it as
// codeRouterBindFailed, the same shape a facade enable's bind failure takes:
// the refusal names the port and invites a different number.
var errRouterBindFailed = errors.New("cluster endpoint bind failed")

// bringUpRouter builds the cluster endpoint for this process: it binds the
// port on loopback, installs the alias document the process is holding,
// serves it until the endpoint is stopped, and asks the broker relay for the
// peer alias feed. The caller decides what a failure costs — the process
// start logs and keeps running dark, the runtime switch refuses without
// disturbing the endpoint that already serves.
//
// Loopback-only is the security posture, not a placeholder: this listener
// routes to any node in the cluster, so exposing it in plaintext to a network
// would hand whoever shares that network a routing oracle for the whole
// cluster. A peer that needs to route across nodes runs its own router on its
// own loopback and hops over the dedicated engine endpoints, which are
// already pin-gated. The listener address is therefore the gate, and no
// per-request RemoteAddr check is added on top of it.
//
// The stop side comes in two shapes on purpose. Teardown (ctx done) drains:
// a capped Shutdown, so handlers unwind before the process moves on. A
// runtime turn-off (stop) does not drain and is not waited on: closing the
// listener takes nothing away from connections already accepted, so a
// generation mid-flight keeps streaming on its open connection while the
// control plane answers at once. That asymmetry is the no-abort invariant;
// http.Server.Shutdown is deliberately absent from the runtime path, because
// its drain-then-cut would wait and then cut.
//
// Port 0 means "let the operating system pick" and is a legitimate input
// here; the protocol default (a payload naming no port takes
// routerDefaultPort) is applied by setRouterEnabled, above this function.
func (p *Proxy) bringUpRouter(ctx context.Context, port int) (*clusterRouter, error) {
	router := newClusterRouter(p, port)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("%w: port %d: %w", errRouterBindFailed, port, err)
	}
	router.ln = ln
	// Install the node's configured aliases before the endpoint can take a
	// request: a router that comes up nameless routes only exact native
	// names, and the courier's next push is not guaranteed to land before a
	// client's first. The document installs as stored — validation lives
	// where the list was written, not here.
	if len(p.clusterAliasDoc) > 0 {
		router.setAliases(p.clusterAliasDoc)
	}
	router.srv = &http.Server{
		Handler:           http.HandlerFunc(router.handle),
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyServerIdleTimeout,
	}
	go func() {
		// A deliberate close surfaces here as ErrServerClosed or an accept
		// failure on the closed listener; either way the stopped mark says
		// so, and only a server that died on its own is worth an error log.
		if err := router.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !router.stopped.Load() {
			slog.Error("cluster router server exited", "addr", address, "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		if !router.stopped.CompareAndSwap(false, true) {
			// A runtime turn-off already closed this endpoint.
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = router.srv.Shutdown(shutdownCtx)
		_ = ln.Close()
	}()
	// The bound address, not the requested one: a router started on port 0 gets
	// a port from the operating system, and this line is the only place the
	// process says which. router/status reports the same number.
	slog.Info("cluster router listening", "addr", ln.Addr().String(), "engine", routerClusterEngine)
	// Ask the broker relay for the peer alias feed. No service filter: every
	// machine that gets this far advertises engine-manager, and the feed's
	// gate is identity (pinned and trusted), not services — a service filter
	// is exactly what hid aliases announced by engine-less peers when this
	// feed shared the per-engine routing subscriptions instead.
	//
	// A failure here is not fatal to the endpoint: the router still routes
	// everything native and everything locally configured, and only loses
	// names other machines announced.
	if err := p.codec.Notify(noderec.MethodSubscribePeerAliases, noderec.SubscribeParams{}); err != nil {
		slog.Warn("cluster router could not subscribe to the peer alias feed; peer-announced aliases will not be read",
			"err", err)
	}
	return router, nil
}

// stop closes the endpoint at runtime: it marks the endpoint stopped (so a
// later status read says "there is none" instead of advertising a port that
// refuses) and closes the listener, which stops new connections without
// touching an accepted one. It never waits, and it is safe to reach again
// after the serve context or another stop already closed the listener.
func (r *clusterRouter) stop() {
	if !r.stopped.CompareAndSwap(false, true) {
		return
	}
	if r.ln == nil {
		return
	}
	addr := r.ln.Addr().String()
	_ = r.ln.Close()
	slog.Info("cluster router stopped", "addr", addr)
}

// routerStatusResult answers the process-scoped router/status read.
type routerStatusResult struct {
	Running bool `json:"running"`
	Port    int  `json:"port"`
}

// status reports the endpoint as this process currently has it.
//
// The port is read off the live listener, never echoed from the configured or
// requested port: the only thing that makes the Endpoints list trustworthy is
// that a row means something answers on that port. A router
// that never bound has no listener, and a router that was stopped keeps
// reporting its closed listener's address; the stopped mark rejects both, so
// the reply follows the socket.
//
// Called on the codec's read goroutine. The listener and the mark are written
// only on that same goroutine (bring-up and stop both apply inline), which is
// why this reads no lock.
func (r *clusterRouter) status() routerStatusResult {
	if r.stopped.Load() || r.ln == nil {
		return routerStatusResult{}
	}
	addr, ok := r.ln.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return routerStatusResult{}
	}
	return routerStatusResult{Running: true, Port: addr.Port}
}

// routerSetEnabledParams is a router/set-enabled payload. Port names the
// number to bind; 0 — a payload that names none — takes routerDefaultPort,
// not "bind nothing" and not "bind whatever a stranger holds".
type routerSetEnabledParams struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

// setRouterEnabled applies a router/set-enabled command, inline on the codec
// read goroutine — the same place process start would have bound the
// endpoint, so there is one bring-up path and one owner of p.clusterRouter.
//
// What a turn-off does: close the listener and return without draining. A
// generation mid-flight keeps streaming on its already-accepted connection;
// new connections get refused. What a rebind does: bind the new port first
// and stop the old listener only once the new one answers, so a refusal
// changes nothing — the endpoint that was serving still serves, and status
// still reports it. Re-applying the state that already holds changes nothing
// either: a redelivered enable keeps the same listener (the broker may resend
// after a reconnect), and a disable on a process that never had an endpoint —
// or already turned one off, including at teardown — is a plain answer.
//
// Rebinding replaces the router whole. The fresh endpoint installs the
// process's stored alias document rather than reading the old router's copy:
// one source for the list keeps a port correction from arriving nameless,
// and the installed set is always what the broker last couriered. A
// bring-up from a dead context is impossible here — the read loop runs
// inside the serve context, so a context that is already done means there is
// no read loop to ask.
func (p *Proxy) setRouterEnabled(params routerSetEnabledParams) (routerStatusResult, error) {
	if !params.Enabled {
		if p.clusterRouter != nil {
			p.clusterRouter.stop()
			p.clusterRouter = nil
		}
		return routerStatusResult{}, nil
	}

	port := params.Port
	if port <= 0 {
		port = routerDefaultPort
	}
	if current := p.clusterRouter; current != nil {
		if !current.stopped.Load() && current.port == port {
			return current.status(), nil
		}
	}

	router, err := p.bringUpRouter(p.serveCtx, port)
	if err != nil {
		// The refusal costs only this call. Leaving the old listener alone is
		// deliberate: moving a working endpoint off its port because a second
		// enable lost a bind race would turn one wrong number into downtime
		// on the path that was fine.
		return routerStatusResult{}, err
	}
	if current := p.clusterRouter; current != nil {
		current.stop()
	}
	p.clusterRouter = router
	return router.status(), nil
}

// handle is the cluster endpoint's whole surface: the model inventory, and
// inference dispatched against the catalog. Anything else is refused locally.
func (r *clusterRouter) handle(w http.ResponseWriter, req *http.Request) {
	// Engine-manager probes a port to decide whether it is talking to an
	// engine or to a compatibility facade. The router is further from being an
	// engine than a facade is, so it refuses rather than forwards: answering a
	// probe with a forwarded response would let engine-manager conclude an
	// engine is present where nothing but a router is listening.
	if req.Header.Get(engineIdentityProbeHeader) == "1" {
		writeIngressError(w, http.StatusConflict, "router-not-engine",
			"the cluster routing endpoint is not an inference engine")
		return
	}

	path := req.URL.Path
	if req.Method == http.MethodGet && path == "/v1/models" {
		r.serveModelList(w)
		return
	}
	if req.Method == http.MethodPost {
		if engines := routerInferenceEngines(path); len(engines) > 0 {
			r.serveInference(w, req, path, engines)
			return
		}
	}

	// Reaching here means the path has no provider to answer for it. That is
	// a different fact from "a provider exists and is down", which the routing
	// path answers with a 502 — hence 404 here.
	writeIngressError(w, http.StatusNotFound, "not-routable",
		"the cluster routing endpoint serves the OpenAI-compatible inference surface and /v1/models only")
}

// routerInferenceEngines names the engines that can be asked to run a request
// for this path, in the dialect the client used.
//
// Two cases, and they come from the same table the facades route with. A path
// in the shared OpenAI/Anthropic surface is declared by every engine, so
// every engine that hosts a requested model is a candidate. A path that only
// Ollama declares (/api/chat and friends) is Ollama's native surface: sending
// it to LM Studio or vLLM would be a request to an endpoint that does not
// exist there, so those providers are not candidates at all.
//
// An empty result means this router does not route this path. That is not an
// invitation to forward it anyway: the facades forward unlisted paths verbatim
// because they front exactly one engine, and the router has no such default.
func routerInferenceEngines(path string) []string {
	var out []string
	for _, profile := range profiles {
		if role, ok := profile.roleFor(http.MethodPost, path); ok && role == roleInferencePOST {
			out = append(out, profile.Name)
		}
	}
	return out
}

// catalogNow returns the catalog to route this request with, rebuilt when the
// cached one has aged past routerCatalogTTL.
func (r *clusterRouter) catalogNow(now time.Time) *routerCatalog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalog != nil && now.Sub(r.catalogAt) < routerCatalogTTL {
		return r.catalog
	}
	catalog := r.buildCatalogLocked()
	r.catalog = catalog
	r.catalogAt = now
	return catalog
}

// buildCatalogLocked projects this node's routing knowledge into one
// multi-engine index.
//
// The endpoints come from the union of the per-engine routing overlays, each
// read through the facade that maintains it. That is where the per-engine
// identity already lives: an overlay entry was admitted for one engine,
// carries that engine's advertised port, and lists the models that engine
// reports. Reconstructing the multi-engine view from those overlays means the
// router inherits the same admission gate the facades apply — local or pinned,
// currently trusted, a dialable address — rather than developing a looser
// second opinion about who is routable. A second gate would drift, and the
// drift would show up as routing to a node the facades refuse to dial.
//
// The announced aliases come from a different source, the relay-fed peer alias
// feed, and that split is the point. A synonym names a model, not the machines
// fronting it, so its validity does not depend on the announcer advertising an
// engine — and a feed that rode the per-engine routing overlays would silently
// drop every alias announced by a peer that currently fronts none. The feed's
// gate is identity (pinned and trusted), applied at replacePeerAliases; the
// endpoints gate stays the overlay's, because dialing still requires an
// engine on a dialable address.
//
// The manual bypass is looked up live at dial time (via the same facade's
// Discovery.IsManual), never trusted from this snapshot, for the same reason
// resolveCandidates resolves pins per request: a cached membership answer is
// stale the moment a node is unpaired.
//
// Caller holds r.mu.
func (r *clusterRouter) buildCatalogLocked() *routerCatalog {
	// One pass over every enabled engine. The count is small and bounded by
	// the profile table, so a flat slice beats a nested map for the lookup
	// this actually needs: name to providers.
	endpoints := make([]routerEndpoint, 0, 16)
	for _, f := range r.host.enabledFacades() {
		for _, n := range f.discovery.Nodes() {
			if n.Port <= 0 {
				continue
			}
			endpoints = append(endpoints, routerEndpoint{
				nodeID:    n.ID,
				engine:    f.profile.Name,
				host:      n.Host,
				addresses: append([]string(nil), n.Addresses...),
				txt:       append([]string(nil), n.TXT...),
				port:      n.Port,
				peerUUID:  n.ClusterUUID,
				manual:    f.discovery.IsManual(n.ID),
				models:    append([]string(nil), n.Models...),
			})
		}
	}
	return newRouterCatalog(endpoints, orderedAliasDefs(r.aliasesLocked(), r.announcedSourcesLocked()))
}

// announcedSourcesLocked projects the peer alias feed into merge sources,
// skipping peers that announced nothing.
//
// Caller holds r.mu. The host display name rides along because the same
// projection answers both "what should the catalog route by" and "where was
// this name written" — the merge ignores it, the alias view reports it.
func (r *clusterRouter) announcedSourcesLocked() []nodeAliasSource {
	announced := make([]nodeAliasSource, 0, len(r.peerAliasNodes))
	for _, n := range r.peerAliasNodes {
		if len(n.Aliases) == 0 {
			continue
		}
		announced = append(announced, nodeAliasSource{
			nodeID:  n.HostUUID,
			host:    n.Name,
			aliases: n.Aliases,
		})
	}
	return announced
}

// routerAliasEntry is one row of the cluster alias view: the synonym, plus the
// peer that announced it when the synonym did not come from this node's own
// configuration. An operator reading the view needs the origin for the same
// reason a misdialed call needs a number — a name they did not write can only
// be changed on the machine that wrote it, so a view without origins would
// send them hunting for which computer to open.
type routerAliasEntry struct {
	noderec.ModelAlias
	// OriginNode and OriginHost name the announcing peer — routing id and
	// display name — and are empty for a locally configured entry, which is
	// how the view distinguishes "mine" from "written elsewhere".
	OriginNode string `json:"originNode,omitempty"`
	OriginHost string `json:"originHost,omitempty"`
}

// routerAliasesResult is the response to the process-scoped "router/get-aliases".
type routerAliasesResult struct {
	Aliases []routerAliasEntry `json:"aliases"`
}

// aliasesView returns the alias set exactly as this node routes by it: the
// catalog's selection — every definition of a name whose model resolves right
// now, each with the origin of the machine that wrote it.
//
// It reads the catalog's selection rows rather than re-merging, because the
// rows already include the names the routing index itself drops: a synonym
// whose model no endpoint here serves is absent from the routing map but kept
// as the view's row, since it is still a name the cluster defined, and on the
// machine that wrote it, it works. View and routing cannot drift by taking
// different opinions about which definitions of a contested name are current —
// they read the same answer, and a name two machines both serve shows the same
// two definitions in the listing, the card, and the provider set.
func (r *clusterRouter) aliasesView() routerAliasesResult {
	catalog := r.catalogNow(time.Now())
	out := make([]routerAliasEntry, len(catalog.aliasEntries))
	copy(out, catalog.aliasEntries)
	return routerAliasesResult{Aliases: out}
}

// aliasesLocked reads the configured alias list. The caller holds r.mu; the
// list is treated as immutable, because setAliases replaces the slice rather
// than mutating one in flight.
func (r *clusterRouter) aliasesLocked() []noderec.ModelAlias {
	return r.aliases
}

// routerSetAliasesParams is the payload of cluster/set-aliases. The alias shape
// is the shared record's, which is also the settings service's: a synonym is
// two strings, so the broker relays the configured list with no translation and
// there is no middle shape to drift from either side.
type routerSetAliasesParams struct {
	Aliases []noderec.ModelAlias `json:"aliases"`
}

// setAliases replaces the configured alias list and takes effect on the very
// next catalog read. Forgetting the cached catalog rather than waiting out
// the TTL is what makes "editing an alias propagates without a restart"
// observable: a configured alias is configuration, and configuration does
// not age in on a cache timer.
func (r *clusterRouter) setAliases(aliases []noderec.ModelAlias) {
	stored := slices.Clone(aliases)
	r.mu.Lock()
	r.aliases = stored
	r.catalog = nil
	r.mu.Unlock()
	slog.Info("cluster router aliases updated", "aliases", len(stored))
}

// replacePeerAliases replaces the alias feed from a cluster:peer-aliases
// snapshot. The snapshot carries the broker's whole directory — every node a
// subscription would see — so this is where the feed's own gate lives: pinned
// and currently trusted peers only. An unverified claim must not be able to
// define a name this router routes by, and this computer's own record is
// dropped because its aliases are configuration (setAliases), not an
// announcement.
//
// Each snapshot is the full set, so the replace is self-correcting: a peer
// that unpins, leaves the cluster, or deletes a name is simply absent (or
// carries a shorter list) in the next snapshot. Like setAliases, the cached
// catalog is dropped rather than aged out, so adding, editing, and removing a
// peer's alias are all observable without waiting on the TTL.
func (r *clusterRouter) replacePeerAliases(params json.RawMessage) {
	var res noderec.GetNodesResult
	if err := json.Unmarshal(params, &res); err != nil {
		slog.Warn("invalid cluster:peer-aliases snapshot", "err", err)
		return
	}
	peers := make([]noderec.DirectoryNode, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		// The scanner guarantees a hostUuid at the browse boundary; one
		// without it cannot be keyed in the merge, so it cannot be read.
		if n.HostUUID == "" {
			continue
		}
		if n.IdentityVerification != noderec.IdentityPinned || !n.Trusted {
			continue
		}
		peers = append(peers, n)
	}
	// The relay sorts by discovery record id; sorting by node id here is what
	// makes orderedAliasDefs' priority order a property of the cluster and not
	// of one directory map's iteration.
	slices.SortFunc(peers, func(a, b noderec.DirectoryNode) int {
		return strings.Compare(a.HostUUID, b.HostUUID)
	})
	r.mu.Lock()
	r.peerAliasNodes = peers
	r.catalog = nil
	r.mu.Unlock()
	slog.Debug("cluster router peer alias feed replaced", "peers", len(peers))
}

// serveModelList answers /v1/models from the catalog.
//
// The list is generated rather than fetched, and that is the point. Routing
// reads the same provider sets, so a name cannot be listed while nothing
// serves it, and cannot stop serving it while it is still listed. A
// fetched-and-merged list would instead depend on which engines happened to
// answer the list request and in what order. It would also have to dedupe on
// digests, and cross-engine digests cannot work: the two engines hosting one
// set of weights advertise different digests for it (a GGUF file hash against
// an Ollama manifest digest), so a digest merge would delete a real provider
// and make the failover set depend on network luck.
//
// Dialability is part of the answer, not an afterthought: a name whose whole
// provider set is unreachable right now is not listed, because the routing
// gate would 502 on it, and the one guarantee that keeps the surface honest
// is that everything listed routes (or is being retried toward routing).
func (r *clusterRouter) serveModelList(w http.ResponseWriter) {
	catalog := r.catalogNow(time.Now())

	type modelRecord struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelRecord, 0, 16)
	for _, name := range catalog.listableNames() {
		if len(r.dialableHops(catalog.resolveProviders(name))) == 0 {
			continue
		}
		// owned_by names the routing class rather than an engine on purpose:
		// at list time the answering engine is not decided, and an "owned_by"
		// guess would read as a routing promise the moment another provider is
		// preferred under load.
		data = append(data, modelRecord{ID: name, Object: "model", OwnedBy: routerClusterEngine})
	}

	body, err := json.Marshal(struct {
		Object string        `json:"object"`
		Data   []modelRecord `json:"data"`
	}{Object: "list", Data: data})
	if err != nil {
		writeIngressError(w, http.StatusInternalServerError, "encode-error",
			"failed to encode the cluster model inventory")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// serveInference dispatches one inference call across whoever provides its
// model.
func (r *clusterRouter) serveInference(w http.ResponseWriter, req *http.Request, path string, dialectEngines []string) {
	reqID := strconv.FormatUint(r.nextRequestID.Add(1), 10)

	bodyBytes, model, bodyErr := bufferBodyAndModel(req, false)
	if bodyErr != nil {
		// Same contract as the facade's ingress path (#79): a body that
		// cannot be read or exceeds the cap never reaches a provider.
		status, message := http.StatusBadRequest, "request body could not be read"
		if errors.Is(bodyErr, errBodyTooLarge) {
			status, message = http.StatusRequestEntityTooLarge, "request body exceeds 32 MiB limit"
		}
		writeIngressError(w, status, "request-body", message)
		return
	}
	if model == "" {
		writeIngressError(w, http.StatusBadRequest, "no-model",
			"a cluster-routed request must name a model")
		return
	}

	catalog := r.catalogNow(time.Now())
	providers := catalog.resolveProviders(model)
	if len(providers) == 0 {
		// Nobody serves this name anywhere. Same local answer and the same
		// non-exception as an unreachable provider set below, kept separate
		// only so the log says which fact the caller should act on.
		slog.Warn("cluster request rejected: no provider serves the requested model",
			"id", reqID, "path", path, "model", model)
		writeIngressError(w, http.StatusBadGateway, "no-provider",
			"no node in this cluster provides the requested model")
		return
	}

	// Dialect before distance: a provider that cannot be asked to speak this
	// path is not a provider for this request, however healthy it is.
	if len(dialectEngines) > 0 {
		filtered := make([]routerProvider, 0, len(providers))
		for _, provider := range providers {
			if slices.Contains(dialectEngines, provider.endpoint.engine) {
				filtered = append(filtered, provider)
			}
		}
		providers = filtered
	}

	hops := r.dialableHops(providers)
	if len(hops) == 0 {
		// The providers exist but none can be dialled right now (unpinned
		// peer, no usable address, engine not enabled here). This is still a
		// local answer with nothing dialled: "I cannot reach anybody who
		// serves this" never becomes "let me try whatever is up".
		// Naming the providers that were considered is the whole diagnostic here:
		// "the cluster says a host serves this and this process cannot dial it"
		// (an unpaired peer, a dead address, an engine disabled here) is a
		// different fix from "nobody serves this name" above.
		considered := make([]string, 0, len(providers))
		for _, provider := range providers {
			considered = append(considered, provider.endpoint.engine+":"+
				provider.endpoint.nodeID+"@"+provider.endpoint.backendHost())
		}
		slog.Warn("cluster request rejected: providers exist but none is dialable",
			"id", reqID, "path", path, "model", model, "providers", considered)
		writeIngressError(w, http.StatusBadGateway, "no-provider",
			"no reachable node in this cluster provides the requested model")
		return
	}

	// The capacity claim is taken after the provider set is known and before
	// anything is dialled, so a request nobody can serve never books load on a
	// node it will not reach.
	hops, held := r.reserveHops(hops)
	r.dispatch(w, req, reqID, model, path, bodyBytes, hops, held)
}

// reserveHops puts this process's shared capacity ledger behind a cluster route:
// the same first-byte demotion and the same reservation the dedicated facades
// take, from the same process-wide map keyed by node id.
//
// That sharing is the point. The scheduler's baseline says a host is busy; a
// dispatch through a facade is a dispatch through that host; and a cluster route
// picking the least-loaded provider has to be counting the requests the facades
// already have in flight, or the router becomes the path that routes a burst onto
// the node everything else just backed away from.
//
// No pin exempts a provider here: facade.SelectedID is a per-engine user
// selection, and a cluster route has no engine to inherit one from, so it passes
// the empty pin and lets load choose.
func (r *clusterRouter) reserveHops(hops []routerHop) ([]routerHop, reservation) {
	p := r.host
	cands := make([]candidate, 0, len(hops))
	for _, hop := range hops {
		cands = append(cands, hop.cand)
	}
	cands = demoteCooling(cands, p.coolingSet(time.Now()))
	cands, held := p.reserveCandidate("", cands)
	if len(cands) != len(hops) {
		// Neither step adds or removes a candidate, so this cannot happen. If it
		// ever did, re-matching by id could drop a provider the resolver said was
		// dialable, and losing a provider is worse than losing the ledger's
		// reordering: route the resolver's own order unreserved.
		return hops, reservation{}
	}

	// Reorder the hops to match the ledger's candidate order. Matching is by node
	// id, and the first unconsumed hop with that id wins, so two providers that
	// somehow share an id cannot be swapped into the wrong slot twice.
	ordered := make([]routerHop, 0, len(hops))
	used := make([]bool, len(hops))
	for _, cand := range cands {
		for i, hop := range hops {
			if !used[i] && hop.cand.id == cand.id {
				used[i] = true
				ordered = append(ordered, hop)
				break
			}
		}
	}
	for i, hop := range hops {
		if !used[i] {
			ordered = append(ordered, hop)
		}
	}
	return ordered, held
}

// dialableHops turns a provider set into dial-ready candidates, in the order
// they should be tried.
//
// The admission rules are resolveCandidates' rules, applied per provider:
// this node's own endpoint is served from its explicit local backend (never
// from this proxy's own listener, which is the self-forward loop that guard
// exists to break), a remote peer is dialed over cluster mTLS only while the
// live mesh still pins it, an explicitly manual node is dialed plain as the
// deliberate bypass it is, and anything else is dropped. Address selection
// reuses the engine facade's own reach chooser rather than a router-private
// one: the router dials exactly the endpoints the facades dial, so a
// confirmed address is the same fact, and a transport error forgets it on the
// one chooser that recorded it.
func (r *clusterRouter) dialableHops(providers []routerProvider) []routerHop {
	p := r.host
	if p.mesh != nil {
		// Re-derive trust for this walk, as resolveCandidates does per
		// request: a peer removed since the catalog was built stops being a
		// routable candidate immediately.
		p.mesh.Refresh()
	}

	// Deterministic baseline order before scheduler priority reorders:
	// catalog iteration order is map order, and routing must not depend on
	// it. (nodeID, engine, native) is a total order on distinct providers.
	ordered := make([]routerProvider, len(providers))
	copy(ordered, providers)
	slices.SortFunc(ordered, func(a, b routerProvider) int {
		if c := strings.Compare(a.endpoint.nodeID, b.endpoint.nodeID); c != 0 {
			return c
		}
		if c := strings.Compare(a.endpoint.engine, b.endpoint.engine); c != 0 {
			return c
		}
		return strings.Compare(a.nativeName, b.nativeName)
	})

	priority := p.PriorityList()
	rank := make(map[string]int, len(priority))
	for i, id := range priority {
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	rankOf := func(id string) int {
		if v, ok := rank[id]; ok {
			return v
		}
		return len(priority)
	}
	// Scheduler priority leads within the provider set, unlisted providers
	// follow in baseline order. The keys are the same node ids the facades'
	// overlays and the reservation ledger use, which is what makes one host's
	// two engines rank the way the scheduler already ranks them.
	slices.SortStableFunc(ordered, func(a, b routerProvider) int {
		return cmp.Compare(rankOf(a.endpoint.nodeID), rankOf(b.endpoint.nodeID))
	})

	hops := make([]routerHop, 0, len(ordered))
	seenHost := make(map[string]bool, len(ordered))
	for _, provider := range ordered {
		endpoint := provider.endpoint
		f := p.facadeFor(endpoint.engine)
		if f == nil {
			// The engine has no facade on this node, so there is nothing that
			// knows how to speak to it — the catalog only indexes enabled
			// engines, so this means one was torn down since the snapshot.
			continue
		}
		node := Node{
			ID:          endpoint.nodeID,
			Host:        endpoint.host,
			Port:        endpoint.port,
			Addresses:   endpoint.addresses,
			TXT:         endpoint.txt,
			ClusterUUID: endpoint.peerUUID,
		}
		u := f.targetURL(node)
		if u == nil {
			continue
		}
		selfPort, aliasBound := f.selfAddresses()
		peerUUID := ""
		switch {
		case isSelfTarget(u, selfPort) || isAnyAliasSelfTarget(u, aliasBound):
			lb, ok := f.localBackendTarget()
			if !ok {
				continue
			}
			u = lb
		case endpoint.peerUUID != "" && p.mesh != nil && p.mesh.HasPin(endpoint.peerUUID):
			u.Scheme = "https"
			peerUUID = endpoint.peerUUID
		case f.discovery.IsManual(endpoint.nodeID):
			// Plain-HTTP bypass, keyed on the live overlay rather than the
			// snapshot flag.
		default:
			continue
		}
		// Defensive, mirroring resolveCandidates: the local backend must
		// never resolve back to this proxy.
		if isSelfTarget(u, selfPort) || isAnyAliasSelfTarget(u, aliasBound) {
			continue
		}
		if seenHost[u.Host] {
			continue
		}
		seenHost[u.Host] = true
		hops = append(hops, routerHop{
			cand:   candidate{id: endpoint.nodeID, url: u, peerUUID: peerUUID},
			engine: endpoint.engine,
			native: provider.nativeName,
			f:      f,
		})
	}
	return hops
}

// dispatch walks the provider set. The contract it keeps from the facade path
// is the one clients depend on: retry only the statuses another node could
// answer differently, never commit a byte to the client and then change its
// mind, and stop rather than widen when the set runs out.
func (r *clusterRouter) dispatch(w http.ResponseWriter, req *http.Request, reqID, model, path string, bodyBytes []byte, hops []routerHop, held reservation) {
	p := r.host
	start := time.Now()

	// Released whichever way this frame ends — committed stream, exhausted set,
	// or the ErrAbortHandler panic a mid-copy upstream death raises, which
	// unwinds straight through the deferred finalize below. A claim kept past the
	// request would keep a node counted as loaded until the next scheduler
	// snapshot, which is exactly the misreport the ledger exists to avoid.
	defer func() { p.releaseReservation(held) }()

	// One job id for the whole walk, so a request that fails over twice is
	// still one job card rather than three. Engine names the routing class,
	// which is what keeps these ids from colliding with either engine's
	// numbering. Admitted requests only: the 400/502 rejections above create
	// no workload record, matching the facades.
	//
	// Everything touching wl here runs on this one goroutine (ModifyResponse
	// and the deferred finalize both unwind through this stack), so no
	// wlMu-style guard is needed — unlike the facade path, which has a
	// disconnect watcher racing with the copy.
	wl := &Workload{
		ID:        reqID,
		Model:     model,
		Engine:    routerClusterEngine,
		RunID:     p.runID,
		State:     "queued",
		CreatedAt: start.UnixMilli(),
		Seq:       1,
	}
	p.emitWorkload(workloadSubmittedMethod, *wl)

	var (
		terminalOnce sync.Once
		proxyErr     string
		status       int
		finalSC      *statusCapture
		servedNode   string
		servedTarget string
	)
	emitTerminal := func(state, errMsg string) {
		terminalOnce.Do(func() {
			now := time.Now().UnixMilli()
			wl.CompletedAt = &now
			wl.State = state
			if errMsg != "" {
				wl.Error = &errMsg
			}
			wl.Seq++
			// Only "completed" gets its own method; every other terminal
			// state, including "cancelled", rides workload:errored — the same
			// wire rule the facade path follows.
			method := workloadCompletedMethod
			if state != "completed" {
				method = workloadErroredMethod
			}
			p.emitWorkload(method, *wl)
		})
	}

	// Deferred for the same reason handleHTTP defers its finalize: a mid-copy
	// upstream death unwinds as panic(http.ErrAbortHandler) past the end of
	// the walk, and a job with no terminal event keeps counting against its
	// node in every scheduler for the life of the process.
	defer func() {
		aborted := recover()

		reportErr := proxyErr
		if aborted != nil && reportErr == "" {
			reportErr = "upstream stream aborted mid-response"
		}
		if finalSC != nil && status == 0 {
			status = finalSC.status
		}

		slog.Debug("cluster router request complete",
			"id", reqID,
			"node_id", servedNode,
			"engine", routerClusterEngine,
			"path", path,
			"target", servedTarget,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"err", reportErr,
		)

		switch {
		case req.Context().Err() != nil:
			emitTerminal("cancelled", "request cancelled before completion")
		case finalSC != nil && finalSC.wroteErr != nil:
			// A dead or half-open client, noticed through the write deadline
			// rather than a context cancel. Classified as cancelled, not
			// failed: nothing went wrong on the provider side.
			emitTerminal("cancelled", "client connection lost: "+finalSC.wroteErr.Error())
		case proxyErr != "" || status >= http.StatusBadRequest:
			msg := proxyErr
			if msg == "" {
				msg = fmt.Sprintf("upstream returned HTTP %d", status)
			}
			emitTerminal("failed", msg)
		default:
			// Includes a 2xx whose copy was cut short upstream: from the
			// moment the stream committed, the truncation is the caller's to
			// resume, not a verdict on the job.
			emitTerminal("completed", "")
		}

		if aborted != nil {
			// Preserve ReverseProxy's contract with http.Server, which recovers
			// ErrAbortHandler and closes the connection.
			panic(aborted)
		}
	}()

	answered := false
	attempts := 0
	for i, hop := range hops {
		if attempts >= maxDispatchAttempts {
			break
		}
		if req.Context().Err() != nil {
			break
		}
		attempts++
		// The final provider's answer is the client's answer, exactly as on the
		// facade path: retrying a status is only worth another dispatch when some
		// provider is still untried, and once none is, forwarding what the last one
		// said beats inventing a verdict of our own. The dispatch budget is
		// deliberately NOT folded in here, unlike the facade's equivalent: a
		// facade's candidate list is one engine's owners, while a cluster provider
		// set can be every host in the cluster, so ending the walk with providers
		// still untried is a case only this path can reach. It is answered after
		// the loop, not by reporting the fifth provider's 503 as the cluster's
		// verdict.
		last := i == len(hops)-1

		// Placement names the node while a dispatch to it is in flight, so
		// the scheduler counts the host that is actually being held. Between
		// attempts it is re-pointed with the next dispatch (or finalized);
		// the gap-clear the facade path does exists to bound its multi-round
		// wait, which this single walk does not have.
		if wl.ScheduledOn != hop.cand.id {
			wl.ScheduledOn = hop.cand.id
			wl.Seq++
			p.emitWorkload(workloadSubmittedMethod, *wl)
		}
		// The capacity claim follows the provider about to be tried, not the one
		// the ledger chose for the round: an attempt can hold a node for the whole
		// first-content budget, and for that time it is the node under load. A
		// same-node move is a no-op. Both this move and the one at the commit point
		// run on this goroutine (ReverseProxy's callbacks are synchronous within
		// ServeHTTP), so the ledger needs no guard here.
		held = p.moveReservation(held, hop.cand.id)

		attemptStart := time.Now()
		attemptCtx, cancelAttempt := context.WithCancel(req.Context())
		retry := false
		// committedAttempt marks the attempt that produced content, which is the
		// only attempt whose silence or byte is evidence for the next request's
		// ordering.
		committedAttempt := false
		sc := &statusCapture{ResponseWriter: w, status: http.StatusOK, idle: idleClientWriteTimeout}

		proxy := &httputil.ReverseProxy{
			Director: func(out *http.Request) {
				out.URL.Scheme = hop.cand.url.Scheme
				out.URL.Host = hop.cand.url.Host
				out.Host = hop.cand.url.Host
			},
			// A remote cluster peer is dialed over mTLS (per-peer pinned
			// transport); self and manual candidates use the plain transport.
			// See candidateTransport.
			Transport: p.candidateTransport(hop.cand),
			ModifyResponse: func(resp *http.Response) error {
				if !last && routerRetryable(resp.StatusCode) {
					retry = true
					return retrySignal{}
				}
				// Hold the commit until the engine produces content, as in
				// the facade path: everything the router routes is
				// inference, so there is no control-path exception.
				body, err := awaitFirstBody(resp.Body, firstBodyTimeout)
				if err != nil {
					proxyErr = err.Error()
					if !last {
						retry = true
						slog.Warn("cluster upstream produced no content, failing over",
							"id", reqID, "node_id", hop.cand.id, "engine", hop.engine,
							"target", hop.cand.url.Host, "path", path, "err", err)
						return retrySignal{}
					}
					slog.Warn("cluster upstream produced no content, no further provider",
						"id", reqID, "node_id", hop.cand.id, "engine", hop.engine,
						"target", hop.cand.url.Host, "path", path, "err", err)
					return err
				}
				resp.Body = body
				// The commit point. From here the client has bytes from this
				// provider and the walk is over either way.
				proxyErr = ""
				servedNode = hop.cand.id
				servedTarget = hop.cand.url.Host
				finalSC = sc
				committedAttempt = true
				// Belt and braces with the pre-dispatch move above: the node that
				// actually served is the one that must carry the load if anything
				// reordered underneath the walk.
				held = p.moveReservation(held, hop.cand.id)
				sc.upstreamAlive = func() { p.reportActivity(hop.cand.id) }
				wl.State = "running"
				startedMs := time.Now().UnixMilli()
				wl.StartedAt = &startedMs
				wl.ScheduledOn = hop.cand.id
				wl.Seq++
				p.emitWorkload(workloadStartedMethod, *wl)
				return nil
			},
			ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
				if _, ok := err.(retrySignal); ok {
					return // the walk advances to the next provider
				}
				if hop.f != nil {
					hop.f.targets.Forget(hop.cand.id)
				}
				proxyErr = err.Error()
				if !last {
					retry = true
					slog.Warn("cluster upstream error, failing over",
						"id", reqID, "node_id", hop.cand.id, "engine", hop.engine,
						"target", hop.cand.url.Host, "path", path, "err", err)
					return
				}
				answered = true
				servedNode = hop.cand.id
				servedTarget = hop.cand.url.Host
				slog.Warn("cluster upstream error, no further provider",
					"id", reqID, "node_id", hop.cand.id, "engine", hop.engine,
					"target", hop.cand.url.Host, "path", path, "err", err)
				body, mErr := json.Marshal(map[string]string{
					"error": "upstream error: " + err.Error(),
				})
				if mErr != nil {
					body = []byte(`{"error":"upstream error"}`)
				}
				// A provider that answered but never produced content timed
				// out rather than failing to be reached, and 504 says so.
				errStatus := http.StatusBadGateway
				if errors.Is(err, errFirstBodyTimeout) {
					errStatus = http.StatusGatewayTimeout
				}
				ew.Header().Set("Content-Type", "application/json")
				ew.Header().Set("X-Content-Type-Options", "nosniff")
				ew.WriteHeader(errStatus)
				ew.Write(body)
			},
		}

		// Each attempt replays the buffered body against a fresh context and,
		// when the provider spells the model differently, a rewritten body.
		attemptReq := req.WithContext(attemptCtx)
		sendBody := bodyBytes
		if hop.native != "" && hop.native != model {
			if rewritten, changed := rewriteRouterModel(bodyBytes, hop.native); changed {
				sendBody = rewritten
			}
		}
		attemptReq.Body = io.NopCloser(bytes.NewReader(sendBody))
		if len(sendBody) != len(bodyBytes) {
			// The rewritten body has a different length than the client's,
			// and the cloned request still carries the original
			// ContentLength. Sending the long number with the short body
			// would hang the upstream read.
			attemptReq.ContentLength = int64(len(sendBody))
			attemptReq.Header.Del("Content-Length")
		}

		proxy.ServeHTTP(sc, attemptReq)
		cancelAttempt()

		// Record how this provider behaved for the next request's ordering, in the
		// ledger the facades read too. Measured rather than inferred from the
		// failure kind: only a provider that got the whole first-content budget and
		// produced nothing is evidence of a stall, and only a byte is evidence that
		// it has recovered. A fast 404 to a stale inventory is deliberately neither,
		// so it neither stamps nor clears.
		if !committedAttempt && time.Since(attemptStart) >= firstBodyTimeout {
			p.noteNoFirstByte(hop.cand.id, time.Now())
		} else if committedAttempt {
			p.noteFirstByte(hop.cand.id)
		}

		if !retry {
			status = sc.status
			return
		}
	}

	if !answered && req.Context().Err() == nil {
		// The only way to reach here is the dispatch budget stopping the walk with
		// providers still untried: a final provider, however it failed, is answered
		// inside the loop (see `last`), and a client that has gone away is excluded
		// by the context test. Reporting the fifth provider's 503 as the cluster's
		// verdict would be wrong twice over — it names one host for a failure the
		// caller cannot act on, and it hides that we never asked anybody else. So
		// this is a local 503, and it is the whole point of D9: exhaustion stops the
		// walk, it never widens it.
		proxyErr = "every cluster provider for this model failed"
		status = http.StatusServiceUnavailable
		slog.Warn("cluster router stopped with providers untried",
			"id", reqID, "path", path, "hops", len(hops), "attempts", attempts,
			"duration_ms", time.Since(start).Milliseconds())
		writeIngressError(w, http.StatusServiceUnavailable, "exhausted", proxyErr)
	}
}

// routerRetryable mirrors the facade path's retry decision for this surface:
// busy/unavailable/gateway statuses, plus a 404 (a listed provider's
// inventory may have gone stale, and every name here arrived through that
// inventory). Genuine client errors (400/401/422…) are not retried — they
// describe the request, so replaying spends a dispatch to collect the same
// answer.
func routerRetryable(code int) bool {
	switch code {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusNotFound:
		return true
	}
	return code >= http.StatusInternalServerError
}

// rewriteRouterModel replaces the body's "model" field with the native
// identifier the chosen provider expects.
//
// A full JSON round trip through map[string]json.RawMessage re-serializes key
// order and may escape non-ASCII punctuation inside string values; both are
// legal JSON and no engine reads meaning into either. The alternative —
// textual surgery on the "model" value — would depend on exact input
// formatting, which is the kind of clever that breaks on a client that
// minifies differently.
//
// Reports unchanged (false) for anything that is not an object carrying a
// string "model": the body then travels as it arrived, because inventing a
// field a client never sent would change the request in a way the provider
// has no way to expect.
func rewriteRouterModel(body []byte, native string) ([]byte, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false
	}
	raw, ok := fields["model"]
	if !ok {
		return nil, false
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil || current == native {
		return nil, false
	}
	encoded, err := json.Marshal(native)
	if err != nil {
		return nil, false
	}
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return out, true
}
