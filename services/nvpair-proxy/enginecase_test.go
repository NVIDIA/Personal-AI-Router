// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The harness the merged suite runs each engine through.
//
// The two proxies previously had parallel suites that drifted: same-named
// tests with different bodies, and behaviors covered on one side only. A
// single table means a case is either declared to apply to both engines or
// explicitly scoped to one, so a gap has to be a decision rather than an
// oversight.
//
// engineCase carries the fixtures a body cannot derive from the profile — the
// paths a client of that engine actually calls, and the shapes its upstream
// returns.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvpair-shared/noderec"
)

type engineCase struct {
	// profile is the engine under test.
	profile engineProfile

	// inferencePath is a path this engine treats as inference, used by bodies
	// that need to drive a routed request.
	inferencePath string

	// nonInferencePath is a real path this engine's clients call that is
	// neither inference nor a model list, so the proxy forwards it verbatim.
	// Bodies use it to prove that model-eligibility filtering and 404
	// failover apply to inference alone. It must stay absent from Routes.
	nonInferencePath string

	// modelListPath is the path whose response the federated list serves.
	modelListPath string

	// emptyModelList is the dialect's empty envelope. It is both what an
	// upstream with no models returns and what the federated list re-emits,
	// which is the assertion: an engine's client must see its own dialect's
	// empty shape rather than a bare 200 or the other engine's.
	emptyModelList string

	// advertisedModel is how a node's discovery inventory spells the model a
	// client reaches with requestedModel. For Ollama the two differ, because
	// an untagged request means the :latest tag.
	advertisedModel string

	// requestedModel is what a client asks for.
	requestedModel string
}

func ollamaCase(t *testing.T) engineCase {
	t.Helper()
	p, ok := profileFor("ollama")
	if !ok {
		t.Fatal("ollama profile missing")
	}
	return engineCase{
		profile:          p,
		inferencePath:    "/api/chat",
		nonInferencePath: "/api/version",
		modelListPath:    "/api/tags",
		emptyModelList:   `{"models":[]}`,
		advertisedModel:  "qwen3:latest",
		requestedModel:   "qwen3",
	}
}

func lmstudioCase(t *testing.T) engineCase {
	t.Helper()
	p, ok := profileFor("lmstudio")
	if !ok {
		t.Fatal("lmstudio profile missing")
	}
	return engineCase{
		profile:       p,
		inferencePath: "/v1/chat/completions",
		// LM Studio's own REST surface, unlisted in Routes and so forwarded
		// verbatim.
		nonInferencePath: "/api/v0/models",
		modelListPath:    "/v1/models",
		emptyModelList:   `{"object":"list","data":[]}`,
		advertisedModel:  "qwen3-8b",
		requestedModel:   "qwen3-8b",
	}
}

func llamacppCase(t *testing.T) engineCase {
	t.Helper()
	p, ok := profileFor("llamacpp")
	if !ok {
		t.Fatal("llamacpp profile missing")
	}
	return engineCase{
		profile:       p,
		inferencePath: "/v1/chat/completions",
		// llama-server's own status surface, unlisted in Routes and so
		// forwarded verbatim.
		nonInferencePath: "/props",
		modelListPath:    "/v1/models",
		emptyModelList:   `{"object":"list","data":[]}`,
		advertisedModel:  "qwen3-8b-gguf",
		requestedModel:   "qwen3-8b-gguf",
	}
}

// advertiseEngine records model on a discovery record as belonging to engine
// p, in whichever per-engine inventory p's eligibility actually reads: the
// installed catalog for the on-demand engines, the loaded set for llama.cpp.
//
// Bodies use this instead of assigning Models or ModelsByEngine directly so a
// shared fixture means "this node can serve model" for every engine, rather
// than silently meaning it only for the ones that consult the catalog.
func advertiseEngine(n *noderec.DirectoryNode, p engineProfile, model string) {
	if p.ModelEligibility == loadedModels {
		if n.LoadedByEngine == nil {
			n.LoadedByEngine = map[string][]string{}
		}
		n.LoadedByEngine[p.Name] = append(n.LoadedByEngine[p.Name], model)
		return
	}
	if n.ModelsByEngine == nil {
		n.ModelsByEngine = map[string][]string{}
	}
	n.ModelsByEngine[p.Name] = append(n.ModelsByEngine[p.Name], model)
	n.Models = append(n.Models, model)
}

// advertise is advertiseEngine for the engine under test.
func (tc engineCase) advertise(n *noderec.DirectoryNode, model string) {
	advertiseEngine(n, tc.profile, model)
}

// inferenceBody is the request body a client sends for requestedModel. It is
// the one shape both dialects share: a top-level "model" field, which is all
// bufferBodyAndModel reads.
func (tc engineCase) inferenceBody() string {
	return fmt.Sprintf(`{"model":%q}`, tc.requestedModel)
}

// inferenceRequest is a routed inference call for requestedModel, the request
// most bodies in the failover suite need.
func (tc engineCase) inferenceRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, tc.inferencePath, strings.NewReader(tc.inferenceBody()))
}

// engineCases is every engine the merged suite exercises. A body that runs
// under this is asserting behavior both proxies must share.
func engineCases(t *testing.T) []engineCase {
	t.Helper()
	return []engineCase{ollamaCase(t), lmstudioCase(t), llamacppCase(t)}
}

// forEachEngine runs body as a subtest per engine.
func forEachEngine(t *testing.T, body func(t *testing.T, tc engineCase)) {
	t.Helper()
	for _, tc := range engineCases(t) {
		t.Run(tc.profile.Name, func(t *testing.T) { body(t, tc) })
	}
}

// anyProfile is for tests whose subject has no engine input at all — transport
// pooling, cluster trust, server timeouts, priority ordering. They need a
// profile only because NewProxy takes one, and running them per engine would
// double the runtime while proving nothing, since there is no branch for the
// second pass to take. Naming that explicitly keeps them from being mistaken
// for coverage gaps.
func anyProfile(t *testing.T) engineProfile {
	t.Helper()
	return ollamaCase(t).profile
}

// anyCase is anyProfile for a body that still needs an engine's fixtures as a
// vehicle — a routed inference request to reach the streaming path — while its
// subject remains engine-free. The zombie suite is the case in point: it tests
// statusCapture's write and flush deadlines and the terminalOnce guard, none of
// which consult the profile. That the path classifies as inference on both
// engines is already covered by the parameterized workload tests, so a second
// pass here would add real-socket seconds and no coverage.
func anyCase(t *testing.T) engineCase {
	t.Helper()
	return ollamaCase(t)
}

// otherEngine returns an engine that is not tc's, for negative fixtures that
// need to prove this proxy ignores a node running only a sibling engine.
func otherEngine(t *testing.T, tc engineCase) engineProfile {
	t.Helper()
	for _, candidate := range engineCases(t) {
		if candidate.profile.Name != tc.profile.Name {
			return candidate.profile
		}
	}
	t.Fatal("no sibling engine to contrast against")
	return engineProfile{}
}

// ollamaOnlyProfile is for bodies that are genuinely Ollama-scoped — the
// OLLAMA_HOST alias, and the native /api/tags dialect LM Studio does not
// serve. Using it rather than engineCases documents that the scoping is
// deliberate.
func ollamaOnlyProfile(t *testing.T) engineProfile {
	t.Helper()
	return ollamaCase(t).profile
}
