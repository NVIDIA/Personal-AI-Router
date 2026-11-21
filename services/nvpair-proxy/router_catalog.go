// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// This file answers one question for the cluster router: which engines, on
// which hosts, can serve a model name a caller used? It is deliberately free
// of transport and trust decisions. It names the (node, engine) pairs that
// provide a model and the identifier each one expects; the router's request
// path decides how — and whether — each pair is dialed, resolving the dial
// class from live trust state the same way (re)resolveCandidates does. That
// separation is what lets one resolution answer both /v1/models and the routing
// of the same request: the listing is generated from these provider sets, so a
// name cannot be listed without resolving, and cannot stop resolving while it
// is still listed.
//
// The resolution order is deliberate:
// exact native identifier, checked across engines, then per-engine naming
// normalization, then a configured alias. Exact always wins, so an alias can
// never shadow a model that was literally renamed to the alias name — people do
// that, and the alias feature must not be able to break it.
//
// An alias is a synonym for a model, not a list of machines,
// so the third pass expands a name into whoever this catalog provides under
// the name it stands for. That is why the same two strings route on every
// computer that hears them: the document says which model, and each computer
// answers with the hosts it can actually see holding it. Several machines may
// define one name, each standing for its own engine's spelling of the same
// weights, and every definition that resolves contributes its hosts to the
// name's provider set — which is what lets one client-side name
// reach any machine currently able to serve it.

import (
	"slices"
	"strconv"
	"strings"

	"nvpair-shared/noderec"
)

// routerEndpoint is one engine's footprint on one host: where that engine
// answers, and which native model identifiers it answers for.
//
// Identity only, and immutable once built. Whether a host is currently dialable
// — pinned and trusted, manual bypass, or this node's own backends — is decided
// per attempt against the live mesh, never cached here, because a cached answer
// goes stale the moment a peer is unpaired or a cluster is left.
type routerEndpoint struct {
	// nodeID is the routing identity: the same key the reservation map, the
	// scheduler's priority list, the cooldown stamps, and a job's scheduledOn
	// carry. Two engines on one host therefore share one capacity claim:
	// the resource under load is the host's GPU, not one
	// engine's view of it.
	nodeID string

	// engine is the engine id from nvpair-shared/engines ("ollama", "vllm", ...),
	// which selects the naming convention a model identifier is matched under and
	// the dialect the client-facing route is spoken in.
	engine string

	// host is the display and dial name for the endpoint. It is not authoritative
	// for reachability: addresses carries what the node published.
	host string

	// addresses are the host addresses this endpoint can be dialed at, in the
	// order the node ranked them. Empty means the entry describes a host we have
	// no usable address for, which is not a routable provider.
	addresses []string

	// txt carries the node's ranked-address records so address selection uses the
	// same ranker (netpick) the facades use, rather than inventing a second one.
	txt []string

	// port is the TCP port that engine answers on for this host.
	port int

	// peerUUID is a remote peer's cluster principal, used to look up the pinned
	// transport. Empty for this node's own backends and for manual entries.
	peerUUID string

	// manual marks an explicitly user-added endpoint, which is dialed plain HTTP
	// as a deliberate bypass rather than over cluster mTLS.
	manual bool

	// models are the native identifiers that engine serves on this host, spelled
	// the way that engine spells them.
	models []string
}

// backendHost renders the endpoint for logs and telemetry: the address and port
// that would be dialed, plus enough context to tell two engines on one host
// apart. It carries no credentials and no request content.
func (e routerEndpoint) backendHost() string {
	host := e.host
	if host == "" && len(e.addresses) > 0 {
		host = e.addresses[0]
	}
	return host + ":" + strconv.Itoa(e.port)
}

// routerProvider is one engine on one host that answers for one model name,
// with the identifier that engine expects to receive. A resolved request yields
// a provider set, not a single winner: failover walks the set, and the set is
// the boundary failover may never widen past.
type routerProvider struct {
	endpoint   routerEndpoint
	nativeName string
}

// routerCatalog is the router's resolution index. Built whole and replaced
// whole: a snapshot either describes the cluster or it does not, and there is
// no per-entry merge that could half-apply a change. That is the same
// replace-on-change shape the facades' discovery overlay uses, and for the same
// reason — drift between two partially-updated views is the failure this avoids.
type routerCatalog struct {
	// native is keyed by the identifier exactly as advertised. Exact lookup is
	// case-sensitive: the engines that match case-sensitively (LM Studio,
	// llama.cpp, vLLM) must keep doing so, so a case-changed identifier is not a
	// provider here any more than it is on a dedicated facade.
	native map[string][]routerProvider

	// normalized is the same provider sets re-keyed under each engine's naming
	// convention, so an Ollama request for "llama3" reaches the host advertising
	// "llama3:latest". It exists only for the second resolution pass; engines
	// whose convention is byte-for-byte matching contribute nothing new to it.
	normalized map[string][]routerProvider

	// alias holds the routed synonyms, already expanded: a name maps to
	// whoever this catalog says provides the model its selected definition
	// stands for. Expansion happens once, at build, so a request never
	// re-resolves a target, and a name whose definitions all resolve nowhere
	// gets no entry at all.
	alias map[string][]routerProvider

	// aliasEntries is one row per defined name — including the names that
	// resolved nowhere and route not at all — carrying the definition the
	// selection picked, origin and all. It exists for the alias view: the
	// index above keeps only what routes, and a synonym that is inert here
	// still works on the machine that wrote it, so the view must not drop it.
	aliasEntries []routerAliasEntry
}

// newRouterCatalog indexes the given endpoints and builds each alias name's
// provider set from every definition of it that resolves. Both inputs are
// copied into the index, so a caller may reuse its slices. Alias definitions
// arrive in priority order from orderedAliasDefs — possibly several per name —
// and one local list per name is enough that no second round of dedupe is
// needed.
func newRouterCatalog(endpoints []routerEndpoint, defs []mergedAlias) *routerCatalog {
	c := &routerCatalog{
		native:     make(map[string][]routerProvider),
		normalized: make(map[string][]routerProvider),
		alias:      make(map[string][]routerProvider),
	}

	for _, endpoint := range endpoints {
		profile, hasProfile := profileFor(endpoint.engine)
		for _, model := range endpoint.models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			provider := routerProvider{endpoint: endpoint, nativeName: model}
			c.native[model] = append(c.native[model], provider)

			// Every provider is filed under its normalized key as well, not only
			// the identifiers that change shape. A request for "llama3" normalizes
			// to "llama3:latest" and has to find the host advertising that longer
			// form verbatim; filing only rewritten names would miss it.
			if hasProfile {
				key := profile.normalizeModel(model)
				c.normalized[key] = append(c.normalized[key], provider)
			}
		}
	}

	// Alias pass. A synonym stands for one model, so it means every host that
	// provides that model: the target runs the exact pass and then every
	// engine's naming convention, which is deliberately wider than a request's
	// own lookup. A caller naming "qwen3" chose that spelling and may mean a
	// different model than somebody's "qwen3:latest", and D1 keeps those apart;
	// an operator writing a target named one set of weights and wants whichever
	// machines hold them, spelled either way.
	//
	// One name may carry several definitions — this node's own plus peers'
	// announcements of the same name — and every definition that resolves
	// right now contributes its providers to the name's set. That is decision
	// D17: the same weights served under different identifiers on different
	// engines are one name's worth of capacity, so an operator points each
	// node's alias at the spelling that node's engine expects and the cluster
	// answers the name from whichever machine is least loaded. Priority order
	// is the order definitions are accumulated in, so a locally configured
	// definition leads its node's own set; it no longer excludes the others,
	// and the walk's real order comes from the scheduler's rank, which leads
	// with the least busy host.
	//
	// A dead definition contributes nothing rather than blocking the rest,
	// which is kept here as a special case: an engine going offline
	// must not blind the cluster to a name a peer can still serve, and the
	// local definition rejoins the set on the build where its model is
	// advertised again, with nothing written on either machine.
	//
	// A definition is skipped — as unusable, not merely beaten — when its
	// target is empty or is itself a defined name. The alias table is never
	// consulted during expansion, so a chain routes no hops anywhere: the
	// broken definition adds nothing to the set, and a chain with no working
	// alternative stays inert. That is also why a target is not looked up as a
	// native name when it happens to be both — resolving one typo by guessing
	// at the next is how a cluster gets a route nobody wrote down.
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		if name := strings.TrimSpace(def.Name); name != "" {
			names[name] = true
		}
	}

	// First-appearance order per name, then each name's definitions in
	// priority order. The order is the answer to "whose turn is it", so it
	// belongs to the merge, never to map iteration.
	var nameOrder []string
	byName := make(map[string][]mergedAlias, len(defs))
	for _, def := range defs {
		name := strings.TrimSpace(def.Name)
		if name == "" {
			continue
		}
		if _, seen := byName[name]; !seen {
			nameOrder = append(nameOrder, name)
		}
		byName[name] = append(byName[name], def)
	}

	for _, name := range nameOrder {
		// Accumulation is deduped by dial, so two definitions that reach the
		// same host through different spellings contribute one attempt rather
		// than two, and the failover walk never visits a provider twice.
		var set []routerProvider
		seen := make(map[string]bool, len(byName[name]))
		var rows []mergedAlias
		for i := range byName[name] {
			def := &byName[name][i]
			target := strings.TrimSpace(def.Model)
			if target == "" || names[target] {
				continue
			}
			if providers := c.providersForTarget(target); len(providers) > 0 {
				set = appendProviders(set, seen, providers)
				rows = append(rows, *def)
			}
		}
		if len(set) == 0 {
			// Inert everywhere: keep the highest-priority definition as the
			// name's single row. It routes nothing here, but it is still a name
			// the cluster defined, and its origin names the machine where it
			// works and where it can be fixed.
			rows = []mergedAlias{byName[name][0]}
		} else {
			c.alias[name] = set
		}
		// One row per contributing definition, in the same priority order the
		// set was accumulated in: a name two machines define legitimately shows
		// two rows, because both of them are answering for it.
		for _, def := range rows {
			c.aliasEntries = append(c.aliasEntries, routerAliasEntry{
				ModelAlias: def.ModelAlias,
				OriginNode: def.originNode,
				OriginHost: def.originHost,
			})
		}
	}
	return c
}

// appendProviders adds the providers of the given sets that out does not
// already hold, keyed by the (host, engine, native identifier) triple.
//
// Two lookups that reach the same triple describe one dial, so the failover
// walk never attempts a route twice because two passes named it. The same
// accumulation is what keeps /v1/models and the routing gate built from one
// answer: the listing walks provider sets, so anything deduped here is missing
// from both, and anything present is in both.
func appendProviders(out []routerProvider, seen map[string]bool, sets ...[]routerProvider) []routerProvider {
	for _, set := range sets {
		for _, provider := range set {
			key := provider.endpoint.nodeID + "\x00" + provider.endpoint.engine + "\x00" + provider.nativeName
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, provider)
		}
	}
	return out
}

// providersForTarget expands an alias's target into whoever this catalog
// provides under that spelling: the exact identifier across every engine, then
// every engine's naming convention, deduped.
//
// Deliberately absent is the alias table. A synonym is resolved once, here, and
// never re-resolved at request time, so a target that is itself a configured
// name — including the alias's own name — yields nothing. Chaining would mean
// a two-hop synonym that no machine can predict from its own catalog, which is
// the opposite of why the document carries a model name instead of a host list.
func (c *routerCatalog) providersForTarget(target string) []routerProvider {
	seen := make(map[string]bool)
	out := appendProviders(nil, seen, c.native[target])
	for _, profile := range profiles {
		out = appendProviders(out, seen, c.normalized[profile.normalizeModel(target)])
	}
	return out
}

// nodeAliasSource is one host's announced alias list as carried by the relay-
// fed peer alias feed. A host configures one list and the feed carries it once
// (a synonym names a model, not one engine's share of it), and the snapshot's
// order is the relay's, not the merge's, so the merge below sorts before it
// contributes: which definitions a shared name is built from, and in what
// order, must not depend on which directory record happened to sort first.
type nodeAliasSource struct {
	nodeID string
	// host is the announcer's display name. The merge itself never routes on
	// it — it is recorded through mergedAlias so an operator can see which
	// machine wrote a name, which is the difference between "somebody's alias
	// routes here" and "I know where to go change it".
	host    string
	aliases []noderec.ModelAlias
}

// mergedAlias is one candidate definition of a name, with the origin of the
// machine that wrote it. The pair says what a name can be used for, the origin
// says whose machine to edit it on — write authority stays with the writer —
// and selection files a view row for each definition that contributes
// providers, so a name several machines serve shows every hand in it.
type mergedAlias struct {
	noderec.ModelAlias
	// originNode and originHost are the announcing peer's routing id and
	// display name, empty for a locally configured entry.
	originNode string
	originHost string
}

// orderedAliasDefs flattens the locally configured list and the peer-announced
// sources into the priority order the alias selection accumulates in: this
// node's own definitions first — its operator typed them, so its definition of
// a shared name leads the name's provider set — then peers in node id order so
// neither the set's order nor its view rows depend on snapshot arrival order,
// each host read once.
//
// Unlike a name-dedupe this keeps several definitions of one name, in order,
// because every definition that resolves right now contributes its providers.
// Within one host's own list a name may appear only once
// (the editor enforces uniqueness; a hand-edited document keeps its first
// entry), so a malformed duplicate can neither double a name's contributions
// nor let a stale definition ride in behind a fresh one from the same machine.
func orderedAliasDefs(local []noderec.ModelAlias, announced []nodeAliasSource) []mergedAlias {
	announced = slices.Clone(announced)
	slices.SortFunc(announced, func(a, b nodeAliasSource) int {
		return strings.Compare(a.nodeID, b.nodeID)
	})

	out := make([]mergedAlias, 0, len(local)+8)
	seenLocal := make(map[string]bool, len(local))
	for _, alias := range local {
		name := strings.TrimSpace(alias.Name)
		if name == "" || seenLocal[name] {
			continue
		}
		seenLocal[name] = true
		out = append(out, mergedAlias{ModelAlias: alias})
	}
	lastHost := ""
	for _, src := range announced {
		// One host should appear once, and the sort puts any second record
		// adjacent regardless; reading each host once anyway means a
		// directory that ever carried two records for one host cannot
		// smuggle a stale alias list in behind the fresh one.
		if src.nodeID == lastHost {
			continue
		}
		lastHost = src.nodeID
		seenName := make(map[string]bool, len(src.aliases))
		for _, alias := range src.aliases {
			name := strings.TrimSpace(alias.Name)
			if name == "" || seenName[name] {
				continue
			}
			seenName[name] = true
			out = append(out, mergedAlias{
				ModelAlias: alias,
				originNode: src.nodeID,
				originHost: src.host,
			})
		}
	}
	return out
}

// resolveProviders returns every provider that can serve a requested model name,
// in resolution order and without duplicates. An empty result means nobody
// provides it, which the request path answers locally with a 502 and no upstream
// contact — it is not a license to try whatever else is reachable.
func (c *routerCatalog) resolveProviders(requested string) []routerProvider {
	name := strings.TrimSpace(requested)
	if name == "" {
		return nil
	}

	// Pass 1: the identifier as spelled, checked against every engine. This
	// short-circuits: a model literally named
	// after an alias keeps routing to itself.
	if exact := c.native[name]; len(exact) > 0 {
		return slices.Clone(exact)
	}

	// Pass 2: the identifier as each engine's naming convention would spell it.
	// Only Ollama rewrites anything, so this pass is what lets an untagged Ollama
	// name resolve; for the case-exact engines it is a lookup of a key they could
	// not have produced differently.
	if byNaming := c.providersForName(name); len(byNaming) > 0 {
		return byNaming
	}

	// Pass 3: the alias table, reached only when nothing provides the name
	// literally. A configured name is a synonym, never a substitute, and its
	// entry is already the expanded provider set, so there is nothing left to
	// resolve here — only a copy to hand out, and a per-request slice is what
	// keeps a caller's failover walk from writing on the catalog.
	return slices.Clone(c.alias[name])
}

// providersForName gathers the convention lookups a request's own spelling
// produces, deduped. Split from the alias expansion (providersForTarget)
// because a request chose its spelling while an alias target did not: this one
// is only consulted after the spelling misses, and it does not include the
// exact pass, which the caller has already tried.
func (c *routerCatalog) providersForName(requested string) []routerProvider {
	seen := make(map[string]bool)
	var out []routerProvider
	for _, profile := range profiles {
		out = appendProviders(out, seen, c.normalized[profile.normalizeModel(requested)])
	}
	return out
}

// listableNames returns every name the router can route, which is the payload
// of its /v1/models. It is built from the same provider sets the routing gate
// reads, and a name enters only if a provider backs it: listing and routing
// cannot disagree because there is nothing for them to disagree about.
//
// Names are deduped by name, not by (name, digest). Two engines hosting the
// same weights advertise different digests — GGUF file hashes against Ollama
// manifest digests — so digest dedupe would delete a real second provider and
// make the failover set depend on which host answered the list first.
func (c *routerCatalog) listableNames() []string {
	names := make([]string, 0, len(c.native)+len(c.alias))
	for name, set := range c.native {
		if len(set) > 0 {
			names = append(names, name)
		}
	}
	// Every alias entry carries a provider set: newRouterCatalog files a synonym
	// only after its target resolves, so there is no empty entry to skip, and the
	// only name dropped here is one a native identifier already claims.
	//
	// One entry per name. Where a name is both a native identifier and an
	// alias, the client gets one row and it resolves to the native provider,
	// matching what a request naming it would do.
	for name := range c.alias {
		if _, native := c.native[name]; native {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
