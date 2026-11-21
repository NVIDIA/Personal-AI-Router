// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Resolution is the routing decision in miniature: which host-and-engine pairs
// answer a name, and which identifier each of them expects to receive. These
// tables fix that answer for the shapes a cluster actually produces — one set of
// weights spelled three ways by three engines, an alias that collides with a
// model somebody literally renamed, a synonym whose target no host here holds —
// because everything downstream follows the provider set: the failover boundary,
// the generated /v1/models, and the model field each host is sent.
//
// A configured alias carries a model name, never a host list, so these tests
// also pin where that name is expanded: once, at catalog build, against the
// engines this catalog happens to index. The same two strings therefore mean the
// same thing on a machine where they route and on one where they do not.
//
// Each test builds its own catalog so the inputs stay visible next to the
// expectation, with hosts named host-a, host-b, ... and each engine's real port
// so a resolved set reads back as a routing answer. Order inside a result is
// part of the expectation: it is the order the dispatch walk follows.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// routerTestEndpoint is one engine's footprint on one host. It carries a dialable
// shape (an address and a port) even though nothing in this file dials: an
// endpoint without either describes a host the router cannot route to.
func routerTestEndpoint(nodeID, engine, host string, port int, models ...string) routerEndpoint {
	return routerEndpoint{
		nodeID:    nodeID,
		engine:    engine,
		host:      host,
		addresses: []string{host},
		port:      port,
		models:    models,
	}
}

// routerTestAlias is a configured alias: a name a caller may use and the one
// model it stands for. Which machines hold that model is deliberately not part
// of the entry — the catalog decides that from the engines it indexes, which is
// what lets one two-line document route on every computer that hears it.
func routerTestAlias(name, target string) noderec.ModelAlias {
	return noderec.ModelAlias{Name: name, Model: target}
}

// localAliasDefs is the candidate list for aliases this node's own operator
// typed: one definition per name, no peer announcements. The catalog selects
// from the same ordered shape the router builds from configuration plus the
// peer feed, so a test writes definitions the production merge would write.
func localAliasDefs(aliases ...noderec.ModelAlias) []mergedAlias {
	return orderedAliasDefs(aliases, nil)
}

// contestedAliasDefs is the candidate list for a name both this node and one
// peer define: the local definition first, then the announced one, in the
// priority order the selection accumulates them in.
func contestedAliasDefs(local, peer noderec.ModelAlias) []mergedAlias {
	return orderedAliasDefs([]noderec.ModelAlias{local}, []nodeAliasSource{
		{nodeID: "peer-1", host: "Peer One", aliases: []noderec.ModelAlias{peer}},
	})
}

// aliasRows renders the selection's view rows as "name=model@origin", with
// "local" standing for a definition this node wrote. One string per definition
// that routes — so a contested name yields two rows when both machines can
// serve theirs — in row order, which pins both which definitions answer for a
// name and whose machine the view says to edit them on.
func aliasRows(entries []routerAliasEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		origin := entry.OriginNode
		if origin == "" {
			origin = "local"
		}
		out = append(out, entry.Name+"="+entry.Model+"@"+origin)
	}
	return out
}

// providerIDs renders a resolved provider set as "node/engine/native" triples.
func providerIDs(providers []routerProvider) []string {
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		out = append(out, provider.endpoint.nodeID+"/"+provider.endpoint.engine+"/"+provider.nativeName)
	}
	return out
}

// TestRouterCatalogResolve_ExactNameFindsEveryEngineThatSpellsItExactly: the
// first pass ignores engine conventions, so every engine advertising this exact
// string joins the provider set, and a request naming it may be answered by
// either host.
func TestRouterCatalogResolve_ExactNameFindsEveryEngineThatSpellsItExactly(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.1", 1234, "qwen3-8b"),
		routerTestEndpoint("host-b", "llamacpp", "10.0.0.2", 8080, "qwen3-8b", "deepseek-r1"),
		// Same weights, advertised under a tag, so it is a different string.
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3-8b:latest"),
	}, nil)

	assert.Equal(t, []string{
		"host-a/lmstudio/qwen3-8b",
		"host-b/llamacpp/qwen3-8b",
	}, providerIDs(catalog.resolveProviders("qwen3-8b")))
}

// TestRouterCatalogResolve_ExactNameOutranksConfiguredAlias: people rename a
// model to an alias they already use, and that rename has to win. A configured
// name is not consulted at all once something spells the request literally, so
// the rename cannot quietly move traffic onto the hosts the alias resolves to.
//
// The alias here names a model another host really does provide, and the same
// alias is checked again without the rename: an empty answer would not tell the
// short-circuit apart from a synonym that never resolved in the first place.
func TestRouterCatalogResolve_ExactNameOutranksConfiguredAlias(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.1", 1234, "fast-model"),
		routerTestEndpoint("host-b", "ollama", "10.0.0.3", 11434, "qwen3:latest"),
	}, localAliasDefs(
		routerTestAlias("fast-model", "qwen3:latest"),
	))

	assert.Equal(t, []string{"host-a/lmstudio/fast-model"},
		providerIDs(catalog.resolveProviders("fast-model")),
		"a synonym answered a name a host spells literally")

	// The control: the same document on the same engine set, with nothing
	// renamed to the alias's name, does route.
	withoutTheRename := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-b", "ollama", "10.0.0.3", 11434, "qwen3:latest"),
	}, localAliasDefs(
		routerTestAlias("fast-model", "qwen3:latest"),
	))
	assert.Equal(t, []string{"host-b/ollama/qwen3:latest"},
		providerIDs(withoutTheRename.resolveProviders("fast-model")),
		"the alias was inert rather than outranked")
}

// TestRouterCatalogResolve_ExactNameDoesNotGainNormalizedProviders names what the
// first pass costs when the same weights are spelled two ways: once some host
// advertises a name literally, the naming-convention pass is not consulted, so
// the Ollama host holding the same weights under ":latest" stays out of the
// failover set. An exact answer that grew providers behind the caller's back
// would make a request's route depend on which host the cluster happened to
// index first. Naming both hosts is what an alias is for, and an alias does
// take both — see ExpandsAnAliasAcrossEverySpellingOfItsTarget.
func TestRouterCatalogResolve_ExactNameDoesNotGainNormalizedProviders(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.1", 1234, "qwen3"),
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3:latest"),
	}, nil)

	assert.Equal(t, []string{"host-a/lmstudio/qwen3"}, providerIDs(catalog.resolveProviders("qwen3")))
}

// TestRouterCatalogResolve_MatchesOllamaNamesIgnoringImpliedTagAndCase: Ollama
// matches untagged and case-folded names, so a caller using one of those reaches
// the host advertising the full form. The provider keeps the advertised
// identifier, because that is the string Ollama itself accepts.
func TestRouterCatalogResolve_MatchesOllamaNamesIgnoringImpliedTagAndCase(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "llama3:latest", "Nemotron:49B"),
	}, nil)

	t.Run("adds the tag an untagged name implies", func(t *testing.T) {
		assert.Equal(t, []string{"host-c/ollama/llama3:latest"},
			providerIDs(catalog.resolveProviders("llama3")))
	})

	t.Run("folds case on both sides of the comparison", func(t *testing.T) {
		assert.Equal(t, []string{"host-c/ollama/Nemotron:49B"},
			providerIDs(catalog.resolveProviders("NEMOTRON:49B")))
	})

	t.Run("resolves a fully qualified name to the advertised short form", func(t *testing.T) {
		assert.Equal(t, []string{"host-c/ollama/llama3:latest"},
			providerIDs(catalog.resolveProviders("registry.ollama.ai/library/llama3:latest")))
	})
}

// TestRouterCatalogResolve_KeepsExactIdentityEnginesCaseSensitive: the engines
// that match identifiers byte-for-byte keep doing so through the cluster
// endpoint. A name spelled differently is not a provider here any more than it
// is on that engine's own port, so a request for it is refused without dialing
// anybody rather than answered by a host whose model name merely looks alike.
func TestRouterCatalogResolve_KeepsExactIdentityEnginesCaseSensitive(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.1", 1234, "qwen3-8b"),
		routerTestEndpoint("host-b", "vllm", "10.0.0.4", 8000, "DeepSeek-R1"),
	}, nil)

	// The baseline that makes the two misses mean something: both endpoints are
	// in the index, and each answers for the name it advertises.
	assert.Equal(t, []string{"host-a/lmstudio/qwen3-8b"},
		providerIDs(catalog.resolveProviders("qwen3-8b")), "an exact name resolves")
	assert.Equal(t, []string{"host-b/vllm/DeepSeek-R1"},
		providerIDs(catalog.resolveProviders("DeepSeek-R1")), "an exact name resolves")

	assert.Empty(t, catalog.resolveProviders("QWEN3-8B"),
		"a case change is not a second provider for an exact-match engine")
	assert.Empty(t, catalog.resolveProviders("deepseek-r1"),
		"a lower-case spelling is not a second provider for an exact-match engine")
}

// TestRouterCatalogResolve_ExpandsAnAliasAcrossEverySpellingOfItsTarget: an
// operator writing a target names a set of weights, not a spelling, so the alias
// pass takes the exact answer and the convention answer together. That is
// deliberately wider than a request's own lookup, which stops at the literal
// spelling: a caller typing "llama3" chose that spelling and may
// mean a different model than somebody else's "llama3:latest".
//
// Each provider is kept under the identifier its engine advertises, because
// that is the string that engine accepts.
func TestRouterCatalogResolve_ExpandsAnAliasAcrossEverySpellingOfItsTarget(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.1", 1234, "llama3"),
		routerTestEndpoint("host-b", "llamacpp", "10.0.0.2", 8080, "llama3"),
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, localAliasDefs(
		routerTestAlias("work-chat", "llama3"),
	))

	providers := catalog.resolveProviders("work-chat")
	assert.Equal(t, []string{
		"host-a/lmstudio/llama3",
		"host-b/llamacpp/llama3",
		"host-c/ollama/llama3:latest",
	}, providerIDs(providers), "a synonym reaches every host that holds the model")

	require.Len(t, providers, 3)
	assert.Equal(t, "10.0.0.3:11434", providers[2].endpoint.backendHost(),
		"the alias reuses the catalog's endpoint for that engine")

	// The contrast that keeps this from reading like a widened request path.
	assert.Equal(t, []string{"host-a/lmstudio/llama3", "host-b/llamacpp/llama3"},
		providerIDs(catalog.resolveProviders("llama3")),
		"the union was applied to a request's own spelling, not to a synonym's target")
}

// TestRouterCatalogResolve_ExpandsATargetWrittenInAShorterSpelling: a synonym for
// an Ollama model written without its implied tag still means that model. A
// target is a reference to a model, not a string to be matched byte-for-byte
// against a catalog, and a person writing "llama3" means the weights a picker
// shows them as "llama3:latest".
func TestRouterCatalogResolve_ExpandsATargetWrittenInAShorterSpelling(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, localAliasDefs(
		routerTestAlias("work-chat", "llama3"),
	))

	assert.Equal(t, []string{"host-c/ollama/llama3:latest"},
		providerIDs(catalog.resolveProviders("work-chat")),
		"a target that only exists under an engine's naming convention resolved to nothing")
}

// TestRouterCatalogResolve_KeepsATargetThatIsAnotherConfiguredNameInert: a
// synonym is expanded once, against the models this catalog holds, and the alias
// table is never part of that expansion. A target that is itself a configured
// name — another synonym, or the alias's own name — names no model, so it yields
// nothing rather than a two-hop route.
//
// Settings reject a chained list where it is written; this is what makes a
// chained document that arrives some other way (a hand-edited file, a peer on an
// older build) inert instead of unpredictable. A followed chain would route one
// name differently on each machine, according to what each happens to index,
// which is the property the two-string shape exists to keep.
func TestRouterCatalogResolve_KeepsATargetThatIsAnotherConfiguredNameInert(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, localAliasDefs(
		routerTestAlias("chat", "llama3:latest"),
		routerTestAlias("fast-chat", "chat"),
		routerTestAlias("loop", "loop"),
	))

	// The control: a target that is a model resolves, so the two empty answers
	// below are the chain being refused, not a catalog that never expands.
	assert.Equal(t, []string{"host-a/ollama/llama3:latest"},
		providerIDs(catalog.resolveProviders("chat")), "a synonym for a model did not route")

	assert.Empty(t, catalog.resolveProviders("fast-chat"),
		"a synonym naming another synonym was followed")
	assert.Empty(t, catalog.resolveProviders("loop"), "a synonym naming itself was followed")

	names := catalog.listableNames()
	assert.Contains(t, names, "chat", "a routable synonym was left out of the listing")
	assert.NotContains(t, names, "fast-chat", "an inert synonym stayed listed")
	assert.NotContains(t, names, "loop", "an inert synonym stayed listed")
}

// TestRouterCatalogResolve_AnswersNothingWhenNoHostHoldsTheTarget: a synonym whose
// model no live endpoint advertises resolves to the empty set, which the request
// path answers locally. "Nobody here provides this" never turns into an attempt
// at whatever else happens to be reachable, and the name is left out of the
// listing for the same reason it is left out of the routing.
func TestRouterCatalogResolve_AnswersNothingWhenNoHostHoldsTheTarget(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, localAliasDefs(
		routerTestAlias("gone", "qwen3:latest"),
	))

	assert.Empty(t, catalog.resolveProviders("gone"))
	assert.NotContains(t, catalog.listableNames(), "gone")
}

// TestRouterCatalogResolve_ListsAHostOnceEvenWhenTwoPassesReachIt: the provider
// set is a set. A host that advertises the target literally also files it under
// its engine's convention key, so the exact and convention lookups overlap and
// several engines share one key. Repeating a (host, engine, identifier) triple
// would spend a second dispatch on one GPU and make the failover boundary a
// count of aliases rather than a count of providers.
func TestRouterCatalogResolve_ListsAHostOnceEvenWhenTwoPassesReachIt(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "llamacpp", "10.0.0.2", 8080, "llama3"),
		routerTestEndpoint("host-b", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, localAliasDefs(
		routerTestAlias("dup", "llama3"),
	))

	assert.Equal(t, []string{
		"host-a/llamacpp/llama3",
		"host-b/ollama/llama3:latest",
	}, providerIDs(catalog.resolveProviders("dup")))
}

// TestRouterCatalogListableNames_ListsEachRoutableNameOnceInSortedOrder: the
// inventory is one row per name, sorted, and built from the same provider sets
// the routing gate reads. Where a name is both a native identifier and a
// configured synonym the caller gets one row (and resolving it finds the native
// provider), and a synonym whose target nobody here holds is not listed at all.
func TestRouterCatalogListableNames_ListsEachRoutableNameOnceInSortedOrder(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.3", 11434, "llama3:latest", "bge-m3:latest"),
		routerTestEndpoint("host-b", "lmstudio", "10.0.0.1", 1234, "llama3-8b", "zephyr"),
	}, localAliasDefs(
		routerTestAlias("chat", "llama3:latest"),
		routerTestAlias("bge-m3:latest", "zephyr"),
		routerTestAlias("gone", "qwen3:latest"),
	))

	assert.Equal(t, []string{"bge-m3:latest", "chat", "llama3-8b", "llama3:latest", "zephyr"},
		catalog.listableNames())

	// The row that a real model already claims answers with that model, which is
	// what a request naming it gets too.
	assert.Equal(t, []string{"host-a/ollama/bge-m3:latest"},
		providerIDs(catalog.resolveProviders("bge-m3:latest")))
}

// TestRouterCatalogListableNames_OmitsNamesThatOnlyResolveThroughAConvention: the
// inventory carries what a cluster advertises, not every string that happens to
// resolve. An untagged Ollama name still routes, and the two do not disagree
// because both read the same provider sets: nothing is listed that would not
// route, and the guarantee is not bidirectional on purpose.
func TestRouterCatalogListableNames_OmitsNamesThatOnlyResolveThroughAConvention(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.3", 11434, "llama3:latest"),
	}, nil)

	assert.NotContains(t, catalog.listableNames(), "llama3")
	assert.Equal(t, []string{"host-a/ollama/llama3:latest"},
		providerIDs(catalog.resolveProviders("llama3")), "the unlisted spelling still routes")
}

// --- alias selection: every definition that can serve the name (D17, D15) ----
//
// One name may carry several definitions — this node's own plus peers'
// announcements of it — and the catalog gives the name the hosts of every
// definition that resolves against the endpoints it indexes right now. These
// fix the transitions that matter: two live definitions make one name's
// capacity the sum of both machines, a definition whose model is offline
// contributes nothing without blocking the others, and it rejoins on the build
// where its model is advertised again, with nothing written on either machine.
// The rows the selection files for the alias view are asserted in the same
// tests, because view and routing must move together.

// TestRouterCatalogResolve_FallsThroughToAPeerDefinitionWhileTheLocalModelIsOffline:
// the local definition stands for a model no endpoint here advertises — the
// engine holding it is stopped — and a peer's definition of the same name
// stands for a model that is live. Routing the name to the peer is the point of
// D15: an engine being offline must not blind the cluster to a name another
// machine can serve.
func TestRouterCatalogResolve_FallsThroughToAPeerDefinitionWhileTheLocalModelIsOffline(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3:4b"),
	}, contestedAliasDefs(
		routerTestAlias("quick-chat", "llama3.2:1b"),
		routerTestAlias("quick-chat", "qwen3:4b"),
	))

	assert.Equal(t, []string{"host-c/ollama/qwen3:4b"},
		providerIDs(catalog.resolveProviders("quick-chat")),
		"a live peer definition did not answer for a name whose local model is offline")
	assert.Contains(t, catalog.listableNames(), "quick-chat",
		"the fallback name was left out of the listing its routing promises")
	assert.Equal(t, []string{"quick-chat=qwen3:4b@peer-1"}, aliasRows(catalog.aliasEntries),
		"the view still attributes the name to the machine whose definition is not routing")
}

// TestRouterCatalogResolve_ContributesEveryLiveDefinitionOfAContestedName pins
// the merge rule: both machines can serve their own definition of the name, so
// the name's capacity is both of them, with the locally configured definition
// leading the set. The view shows two rows because both machines really are
// answering for the name.
func TestRouterCatalogResolve_ContributesEveryLiveDefinitionOfAContestedName(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.2", 11434, "llama3.2:1b"),
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3:4b"),
	}, contestedAliasDefs(
		routerTestAlias("quick-chat", "llama3.2:1b"),
		routerTestAlias("quick-chat", "qwen3:4b"),
	))

	assert.Equal(t, []string{"host-a/ollama/llama3.2:1b", "host-c/ollama/qwen3:4b"},
		providerIDs(catalog.resolveProviders("quick-chat")),
		"a name both machines define reached only one of them")
	assert.Equal(t, []string{"quick-chat=llama3.2:1b@local", "quick-chat=qwen3:4b@peer-1"},
		aliasRows(catalog.aliasEntries),
		"the view hid a machine that is answering for the name")
}

// TestRouterCatalogResolve_ServesOneNameFromDifferentIdentifiersOnDifferentEngines
// is the case the routing decision exists for: the same weights packed twice,
// each engine calling them by its own name, so neither node's document can name
// what the other advertises. Two definitions of one name, each spelled for its
// own engine, and the client-side name reaches both machines.
func TestRouterCatalogResolve_ServesOneNameFromDifferentIdentifiersOnDifferentEngines(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "lmstudio", "10.0.0.2", 1234, "qwen/Qwen3.8-Flash-Next-Q4_K_XL"),
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen38-flash-next-q4:latest"),
	}, contestedAliasDefs(
		routerTestAlias("qwen38-flash-next", "qwen/Qwen3.8-Flash-Next-Q4_K_XL"),
		routerTestAlias("qwen38-flash-next", "qwen38-flash-next-q4:latest"),
	))

	assert.Equal(t,
		[]string{"host-a/lmstudio/qwen/Qwen3.8-Flash-Next-Q4_K_XL", "host-c/ollama/qwen38-flash-next-q4:latest"},
		providerIDs(catalog.resolveProviders("qwen38-flash-next")),
		"one name did not reach both packings of the same weights")
	assert.Contains(t, catalog.listableNames(), "qwen38-flash-next",
		"a name two machines serve was left out of the listing its routing promises")
}

// TestRouterCatalogResolve_CountsTwoDefinitionsReachingOneProviderOnce keeps the
// failover walk honest: two definitions that end up at the same host are one
// dial, not two attempts on it, so a name whose definitions overlap does not
// spend its dispatch budget re-asking a machine that already answered.
func TestRouterCatalogResolve_CountsTwoDefinitionsReachingOneProviderOnce(t *testing.T) {
	same := routerTestAlias("quick-chat", "llama3.2:1b")
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.2", 11434, "llama3.2:1b"),
	}, contestedAliasDefs(same, same))

	assert.Equal(t, []string{"host-a/ollama/llama3.2:1b"},
		providerIDs(catalog.resolveProviders("quick-chat")),
		"two definitions of one name reached the same provider twice")
	assert.Len(t, catalog.aliasEntries, 2,
		"the view dropped a machine whose definition is real even though it dials nothing extra")
}

// TestRouterCatalogResolve_AddsTheLocalDefinitionBackWhenItsModelIsAdvertisedAgain:
// the same definitions, two builds — the local engine offline, then advertising
// its model again. The name loses the offline machine and gains it back on the
// next build, with no configuration changing anywhere: what moves is which
// definitions resolve, and a definition that resolves joins the set rather than
// taking a name back from anybody.
func TestRouterCatalogResolve_AddsTheLocalDefinitionBackWhenItsModelIsAdvertisedAgain(t *testing.T) {
	defs := contestedAliasDefs(
		routerTestAlias("quick-chat", "llama3.2:1b"),
		routerTestAlias("quick-chat", "qwen3:4b"),
	)
	peerLive := []routerEndpoint{
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3:4b"),
	}

	offline := newRouterCatalog(peerLive, defs)
	assert.Equal(t, []string{"host-c/ollama/qwen3:4b"},
		providerIDs(offline.resolveProviders("quick-chat")),
		"the name stayed inert while the local model was offline")
	assert.Equal(t, []string{"quick-chat=qwen3:4b@peer-1"}, aliasRows(offline.aliasEntries),
		"the view listed a machine whose model nobody advertises")

	back := newRouterCatalog(append([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.2", 11434, "llama3.2:1b"),
	}, peerLive...), defs)
	assert.Equal(t, []string{"host-a/ollama/llama3.2:1b", "host-c/ollama/qwen3:4b"},
		providerIDs(back.resolveProviders("quick-chat")),
		"the local definition stayed out of the name after its model was advertised again")
	assert.Equal(t, []string{"quick-chat=llama3.2:1b@local", "quick-chat=qwen3:4b@peer-1"},
		aliasRows(back.aliasEntries),
		"the view still showed only the handover after the local machine came back")
}

// TestRouterCatalogResolve_TakesTheFirstLiveDefinitionFromAnyHost: walking past
// definitions that resolve nowhere is not a local-versus-peer rule. Here the
// local definition and the first peer's both stand for models nobody
// advertises, and the second peer's live definition gets the turn.
func TestRouterCatalogResolve_TakesTheFirstLiveDefinitionFromAnyHost(t *testing.T) {
	defs := orderedAliasDefs(
		[]noderec.ModelAlias{routerTestAlias("quick-chat", "llama3.2:1b")},
		[]nodeAliasSource{
			{nodeID: "host-b", host: "Host B", aliases: []noderec.ModelAlias{
				{Name: "quick-chat", Model: "deepseek-r1"},
			}},
			{nodeID: "host-c", host: "Host C", aliases: []noderec.ModelAlias{
				{Name: "quick-chat", Model: "qwen3:4b"},
			}},
		},
	)
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-c", "ollama", "10.0.0.3", 11434, "qwen3:4b"),
	}, defs)

	assert.Equal(t, []string{"host-c/ollama/qwen3:4b"},
		providerIDs(catalog.resolveProviders("quick-chat")))
	assert.Equal(t, []string{"quick-chat=qwen3:4b@host-c"}, aliasRows(catalog.aliasEntries),
		"the view names a machine whose definition is not the one routing")
}

// TestRouterCatalogResolve_KeepsTheHighestPriorityDefinitionWhenAllResolveNowhere:
// when no definition routes, nothing is listed and nothing dials, but the view
// still gets a row — the highest-priority definition, the one this node's own
// operator typed — because it says what the name means and where it works.
func TestRouterCatalogResolve_KeepsTheHighestPriorityDefinitionWhenAllResolveNowhere(t *testing.T) {
	catalog := newRouterCatalog([]routerEndpoint{
		routerTestEndpoint("host-a", "ollama", "10.0.0.2", 11434, "llama3:latest"),
	}, contestedAliasDefs(
		routerTestAlias("gone", "qwen3:latest"),
		routerTestAlias("gone", "deepseek-r1"),
	))

	assert.Empty(t, catalog.resolveProviders("gone"))
	assert.NotContains(t, catalog.listableNames(), "gone")
	assert.Equal(t, []string{"gone=qwen3:latest@local"}, aliasRows(catalog.aliasEntries),
		"the row for an inert name was taken by the peer's definition instead")
}

// --- orderedAliasDefs ---------------------------------------------------------
//
// The merge decides the order the selection above walks when one name has
// several definitions — this node's own first, then peers in node id order —
// and how many definitions a name gets to begin with. Which of them routes is
// resolution, pinned by the tests just above; what these fix is the order and
// the set. A definition is two names, so what is ordered is whose turn a name
// stands for — never which machines hold a model, which each computer answers
// for itself.

func TestOrderedAliasDefs_ListsTheLocalDefinitionBeforeAPeerDefinitionOfTheSameName(t *testing.T) {
	got := orderedAliasDefs(
		[]noderec.ModelAlias{routerTestAlias("quick-chat", "llama3.2:1b")},
		[]nodeAliasSource{
			{nodeID: "host-b", host: "Host B", aliases: []noderec.ModelAlias{
				{Name: "quick-chat", Model: "qwen3:4b"},
				{Name: "embeddings", Model: "nomic-embed"},
			}},
		},
	)

	// Both definitions of "quick-chat" survive, local first: the peer's stays
	// behind it as the definition the selection can reach when the local one
	// resolves nowhere. "embeddings" is new and rides along behind.
	require.Len(t, got, 3, "a contested name was deduped to one definition before selection")
	assert.Equal(t, routerTestAlias("quick-chat", "llama3.2:1b"), got[0].ModelAlias)
	assert.Empty(t, got[0].originNode,
		"a locally configured entry was marked as somebody else's announcement")
	assert.Equal(t, routerTestAlias("quick-chat", "qwen3:4b"), got[1].ModelAlias)
	assert.Equal(t, "host-b", got[1].originNode,
		"the peer's definition carries no origin, so a reader cannot tell where to edit it")
	assert.Equal(t, "Host B", got[1].originHost)
	assert.Equal(t, routerTestAlias("embeddings", "nomic-embed"), got[2].ModelAlias)
}

func TestOrderedAliasDefs_PeerDefinitionsTakeTheirTurnsInSortedNodeOrder(t *testing.T) {
	// Two peers stand for "fast-chat" differently. Snapshots arrive in the
	// relay's order, not the merge's, so the merge sorts its inputs first:
	// host-b arrives first here and must still take its turn after host-a. If
	// whose definition was tried first followed arrival order, two computers
	// given the same cluster picture could route one name to two different
	// models.
	announced := []nodeAliasSource{
		{nodeID: "host-b", host: "Host B", aliases: []noderec.ModelAlias{
			{Name: "fast-chat", Model: "model-b"},
		}},
		{nodeID: "host-a", host: "Host A", aliases: []noderec.ModelAlias{
			{Name: "fast-chat", Model: "model-a"},
		}},
	}

	got := orderedAliasDefs(nil, announced)

	require.Len(t, got, 2, "a contested name was collapsed to one definition before selection")
	assert.Equal(t, routerTestAlias("fast-chat", "model-a"), got[0].ModelAlias,
		"the first turn went to the host that arrives first, not the host that sorts first")
	assert.Equal(t, "host-a", got[0].originNode)
	assert.Equal(t, "host-b", got[1].originNode)
}

func TestOrderedAliasDefs_OneHostContributesItsListOnceEvenInTwoSnapshotRecords(t *testing.T) {
	// The relay directory keys records by discovery record id, so a host could
	// in principle arrive twice in one snapshot. The merge reads each host
	// once; a second record must neither duplicate a name nor queue a stale
	// definition of it behind the fresh one, which would let an out-of-date
	// model answer as soon as the fresh one went offline.
	full := []noderec.ModelAlias{
		{Name: "fast-chat", Model: "model-x"},
		{Name: "embeddings", Model: "model-y"},
	}
	got := orderedAliasDefs(nil, []nodeAliasSource{
		{nodeID: "host-x", host: "Host X", aliases: full},
		{nodeID: "host-x", host: "Host X", aliases: []noderec.ModelAlias{
			{Name: "fast-chat", Model: "stale-model"},
		}},
	})

	require.Len(t, got, 2, "the duplicate record doubled the list")
	assert.Equal(t, full, []noderec.ModelAlias{got[0].ModelAlias, got[1].ModelAlias},
		"the duplicate record got its definition of a name read")
}

func TestOrderedAliasDefs_KeepsOnlyTheFirstOfTwoLocalDefinitionsOfOneName(t *testing.T) {
	// The editor refuses two rows with one name, so a duplicate arrives only
	// from a hand-edited document. One local claim per name is enough: a
	// second would take a later turn than any peer's definition of the same
	// name, a precedence nobody wrote down.
	got := orderedAliasDefs([]noderec.ModelAlias{
		routerTestAlias("quick-chat", "llama3.2:1b"),
		routerTestAlias("quick-chat", "qwen3:4b"),
	}, nil)

	require.Len(t, got, 1, "a hand-edited duplicate gave one machine two turns for a name")
	assert.Equal(t, routerTestAlias("quick-chat", "llama3.2:1b"), got[0].ModelAlias)
}
