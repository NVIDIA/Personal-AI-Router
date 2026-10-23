// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
)

func TestProxyProfiles_MatchCanonicalEngineList(t *testing.T) {
	names := make([]string, len(profiles))
	for i, profile := range profiles {
		names[i] = profile.Name
	}
	require.Equal(t, engines.Names(), names, "proxy profiles must match the canonical engines")
}

func TestLlamaCPPProfile_DeclaresChatRouteAndReservedEnginePort(t *testing.T) {
	profile, ok := profileFor("llamacpp")
	require.True(t, ok, "llamacpp profile missing")
	role, ok := profile.roleFor("POST", "/v1/chat/completions")
	require.True(t, ok, "chat route missing")
	require.Equal(t, roleInferencePOST, role, "chat route")
	require.Equal(t, "org/model:Q4_K_M", profile.normalizeModel("org/model:Q4_K_M"), "model identifiers must match exactly")
	require.Equal(t, 8080, profile.StandalonePort, "standalone facade port")
	require.Equal(t, 8081, profile.ReservedPersistedPort, "reserved engine port")
}

func TestLlamaCPPModelList_UsesNativeInventoryForBothPublicRoutes(t *testing.T) {
	profile, ok := profileFor("llamacpp")
	require.True(t, ok, "llamacpp profile missing")
	test := func(name, path string) {
		t.Run(name, func(t *testing.T) {
			route, ok := profile.routeFor(http.MethodGet, path)
			require.True(t, ok, "model list route is not classified")
			require.Equal(t, roleModelListOpenAIGET, route.Role, "model list route")
			require.Equal(t, "/models", route.upstreamPath(), "upstream model list path")
		})
	}
	test("native model list", "/models")
	test("OpenAI model list", "/v1/models")
}

// Routes is a classifier, not an allowlist. handlePlain forwards every
// loopback path into handleHTTP with no filtering, so a path the table does
// not mention must still reach the upstream verbatim — that is how /api/pull,
// /api/ps, /api/version and OPTIONS preflights keep working.
func TestRoleFor_ClassifiesOnlyDeclaredRoutes(t *testing.T) {
	ollama, ok := profileFor("ollama")
	require.True(t, ok, "ollama profile missing")
	lmstudio, ok := profileFor("lmstudio")
	require.True(t, ok, "lmstudio profile missing")
	llamacpp, ok := profileFor("llamacpp")
	require.True(t, ok, "llamacpp profile missing")

	accept := func(name string, profile engineProfile, method, path string, wantRole routeRole) {
		t.Run(name, func(t *testing.T) {
			role, ok := profile.roleFor(method, path)
			require.True(t, ok, "roleFor(%s %s) did not classify a declared route", method, path)
			require.Equal(t, wantRole, role, "roleFor(%s %s)", method, path)
		})
	}
	reject := func(name string, profile engineProfile, method, path string) {
		t.Run(name, func(t *testing.T) {
			role, ok := profile.roleFor(method, path)
			require.False(t, ok, "roleFor(%s %s) classified an undeclared route with role %v", method, path, role)
		})
	}

	accept("ollama native chat", ollama, http.MethodPost, "/api/chat", roleInferencePOST)
	accept("ollama openai chat", ollama, http.MethodPost, "/v1/chat/completions", roleInferencePOST)
	accept("ollama openai responses", ollama, http.MethodPost, "/v1/responses", roleInferencePOST)
	accept("ollama anthropic messages", ollama, http.MethodPost, "/v1/messages", roleInferencePOST)
	accept("ollama native list", ollama, http.MethodGet, "/api/tags", roleModelListNativeGET)
	accept("ollama openai list", ollama, http.MethodGet, "/v1/models", roleModelListOpenAIGET)
	accept("ollama show", ollama, http.MethodPost, "/api/show", roleModelLookupPOST)

	accept("lmstudio chat", lmstudio, http.MethodPost, "/v1/chat/completions", roleInferencePOST)
	accept("lmstudio responses", lmstudio, http.MethodPost, "/v1/responses", roleInferencePOST)
	accept("lmstudio anthropic messages", lmstudio, http.MethodPost, "/v1/messages", roleInferencePOST)
	accept("lmstudio list", lmstudio, http.MethodGet, "/v1/models", roleModelListOpenAIGET)
	accept("lmstudio openai responses", lmstudio, http.MethodPost, "/v1/responses", roleInferencePOST)

	accept("llamacpp responses", llamacpp, http.MethodPost, "/v1/responses", roleInferencePOST)
	accept("llamacpp openai responses", llamacpp, http.MethodPost, "/v1/responses", roleInferencePOST)

	reject("ollama passthrough", ollama, http.MethodPost, "/api/pull")
	reject("ollama version passthrough", ollama, http.MethodGet, "/api/version")
	reject("ollama models passthrough", ollama, http.MethodGet, "/models")
	reject("wrong method on list", ollama, http.MethodPost, "/v1/models")
	reject("wrong method on inference", ollama, http.MethodGet, "/api/chat")
	reject("wrong method on responses", ollama, http.MethodGet, "/v1/responses")
	reject("wrong method on show", ollama, http.MethodGet, "/api/show")

	reject("lmstudio models passthrough", lmstudio, http.MethodGet, "/models")
	// LM Studio serves no native Ollama routes, so /api/chat is forwarded verbatim.
	reject("lmstudio has no native routes", lmstudio, http.MethodPost, "/api/chat")
	reject("lmstudio has no show", lmstudio, http.MethodPost, "/api/show")

	reject("llamacpp models post passthrough", llamacpp, http.MethodPost, "/models")
}

func TestIsInferenceRequest_FollowsTheProfile(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	assert.True(t, isInferenceRequest(ollama, "POST", "/api/generate"), "ollama /api/generate must be inference")
	assert.True(t, isInferenceRequest(ollama, "POST", "/v1/responses"), "ollama /v1/responses must be inference so model-owner filtering applies")
	assert.True(t, isInferenceRequest(lmstudio, "POST", "/v1/responses"), "lmstudio /v1/responses must be inference so model-owner filtering applies")
	assert.False(t, isInferenceRequest(lmstudio, "POST", "/api/generate"), "lmstudio serves no native routes, so /api/generate is not inference for it")
	assert.False(t, isInferenceRequest(ollama, "GET", "/api/tags"), "a model list is not inference")
	assert.False(t, isInferenceRequest(ollama, "POST", "/api/show"), "a model lookup is not inference")
}

// One naming convention governs both the federated list's dedupe key and the
// routing gate. If they disagreed the proxy could advertise a model it then
// refuses to route.
func TestModelNames_FollowEngineNormalizationRules(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	test := func(name string, profile engineProfile, in, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, profile.normalizeModel(in))
		})
	}
	test("Ollama default tag", ollama, "llama3", "llama3:latest")
	test("Ollama explicit default tag", ollama, "llama3:latest", "llama3:latest")
	test("Ollama explicit tag", ollama, "llama3:8b", "llama3:8b")
	test("Ollama registry prefix", ollama, "registry.example/llama3", "registry.example/llama3:latest")
	test("Ollama digest", ollama, "llama3@sha256:abc", "llama3@sha256:abc")
	test("Ollama library namespace", ollama, "library/qwen3:latest", "qwen3:latest")
	test("Ollama default registry and library", ollama, "registry.ollama.ai/library/qwen3:latest", "qwen3:latest")
	test("Ollama registry URL", ollama, "https://registry.ollama.ai/library/qwen3", "qwen3:latest")
	test("Ollama case normalization", ollama, "Qwen3", "qwen3:latest")
	test("Ollama explicit registry without namespace", ollama, "registry.ollama.ai/qwen3", "registry.ollama.ai/qwen3:latest")
	test("Ollama custom registry case normalization", ollama, "hf.co/Org/Model:Q4_K_M", "hf.co/org/model:q4_k_m")
	test("Ollama registry port", ollama, "localhost:5000/ns/m", "localhost:5000/ns/m:latest")
	test("Ollama empty name", ollama, "", "")
	test("LM Studio untagged name", lmstudio, "qwen3-8b", "qwen3-8b")
	test("LM Studio exact tag", lmstudio, "qwen3-8b:latest", "qwen3-8b:latest")
	test("LM Studio preserves case", lmstudio, "Qwen3-8B", "Qwen3-8B")
	test("LM Studio empty name", lmstudio, "", "")
}

func TestNodeAdvertisesModel_UsesEngineSpecificModelNames(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	tagged := Node{Models: []string{"llama3:latest"}}
	assert.True(t, nodeAdvertisesModel(ollama, tagged, "llama3"), "ollama must treat an untagged request as the :latest tag")
	assert.False(t, nodeAdvertisesModel(lmstudio, tagged, "llama3"), "lmstudio matches identifiers exactly, so llama3 is not llama3:latest")
	assert.False(t, nodeAdvertisesModel(ollama, Node{Models: []string{"llama3:latest"}}, ""), "an empty request model advertises nothing")
}

// A path may legitimately be declared twice under different methods. roleFor
// used to return on the first path match, which made the second entry
// unreachable — it would be forwarded verbatim instead of classified, and
// nothing would say so.
func TestRoleFor_FindsAPathDeclaredUnderTwoMethods(t *testing.T) {
	p := engineProfile{Routes: []route{
		{Path: "/v1/models", Role: roleInferencePOST},
		{Path: "/v1/models", Role: roleModelListOpenAIGET},
	}}

	role, ok := p.roleFor("GET", "/v1/models")
	assert.True(t, ok, "GET /v1/models (%v, %v)", role, ok)
	assert.Equal(t, roleModelListOpenAIGET, role, "GET /v1/models (%v, %v)", role, ok)
	role, ok = p.roleFor("POST", "/v1/models")
	assert.True(t, ok, "POST /v1/models (%v, %v)", role, ok)
	assert.Equal(t, roleInferencePOST, role, "POST /v1/models (%v, %v)", role, ok)
	_, ok = p.roleFor("DELETE", "/v1/models")
	assert.False(t, ok, "DELETE /v1/models classified; an undeclared method must forward verbatim")
}

// No shipped engine declares the same (path, method) twice; a duplicate would
// make whichever entry came second dead.
func TestEngineProfiles_DeclareEachMethodAndPathOnce(t *testing.T) {
	for _, p := range profiles {
		seen := map[string]bool{}
		for _, r := range p.Routes {
			key := r.Role.method() + " " + r.Path
			assert.NotContains(t, seen, key)
			seen[key] = true
		}
	}
}

// The error-ID prefix derives from Name so it cannot disagree with the
// broker's expectations. The persisted-port filename deliberately does NOT
// derive: Ollama's predates the multi-engine world, and deriving it would
// rename the file under every existing install and orphan the port a user set.
func TestEngineProfiles_PreservePortFilesAndDeriveErrorIdentifiers(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	assert.Equal(t, "proxy-port.json", ollama.PortFile, "ollama PortFile")
	assert.Equal(t, "lmstudio-proxy-port.json", lmstudio.PortFile, "lmstudio PortFile")
	assert.Equal(t, "ollama-proxy:upstream-unreachable:peer-A", upstreamUnreachableID(ollama, "peer-A"), "ollama upstreamUnreachableID")
	assert.Equal(t, "lmstudio-proxy:upstream-unreachable:peer-A", upstreamUnreachableID(lmstudio, "peer-A"), "lmstudio upstreamUnreachableID")
}

// chooseStartupPort restores a previously chosen port, except when the broker
// forces one or when the stored value belongs to the engine itself.
func TestChooseStartupPort_RespectsOverridesAndReservedPorts(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	for _, tc := range []struct {
		name            string
		profile         engineProfile
		flagPort        int
		ignorePersisted bool
		persisted       int
		hasPersisted    bool
		want            int
	}{
		{"restores persisted", ollama, 11435, false, 11500, true, 11500},
		{"broker override wins", ollama, 11434, true, 11500, true, 11434},
		{"no persisted value", ollama, 11435, false, 0, false, 11435},
		{"lmstudio restores persisted", lmstudio, 1234, false, 1300, true, 1300},
		// 1235 is where engine-manager runs a managed LM Studio, so restoring
		// it would put the proxy on the engine's own port.
		{"lmstudio refuses the engine's port", lmstudio, 1234, false, 1235, true, 1234},
		// Ollama reserves nothing, so the equivalent value is honoured.
		{"ollama reserves nothing", ollama, 11434, false, 11435, true, 11435},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseStartupPort(tc.profile, tc.flagPort, tc.ignorePersisted, tc.persisted, tc.hasPersisted)
			require.Equal(t, tc.want, got, "chooseStartupPort (%v)", got)
		})
	}
}
