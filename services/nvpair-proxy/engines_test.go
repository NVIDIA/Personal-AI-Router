// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// Routes is a classifier, not an allowlist. handlePlain forwards every
// loopback path into handleHTTP with no filtering, so a path the table does
// not mention must still reach the upstream verbatim — that is how /api/show,
// /api/pull, /api/ps, /api/version and OPTIONS preflights keep working.
func TestRoleForClassifiesOnlyDeclaredRoutes(t *testing.T) {
	ollama, ok := profileFor("ollama")
	if !ok {
		t.Fatal("ollama profile missing")
	}
	lmstudio, ok := profileFor("lmstudio")
	if !ok {
		t.Fatal("lmstudio profile missing")
	}

	for _, tc := range []struct {
		name     string
		profile  engineProfile
		method   string
		path     string
		wantRole routeRole
		wantOK   bool
	}{
		{"ollama native chat", ollama, "POST", "/api/chat", roleInferencePOST, true},
		{"ollama openai chat", ollama, "POST", "/v1/chat/completions", roleInferencePOST, true},
		{"ollama native list", ollama, "GET", "/api/tags", roleModelListNativeGET, true},
		{"ollama openai list", ollama, "GET", "/v1/models", roleModelListOpenAIGET, true},
		{"ollama passthrough", ollama, "POST", "/api/pull", 0, false},
		{"ollama version passthrough", ollama, "GET", "/api/version", 0, false},

		{"lmstudio chat", lmstudio, "POST", "/v1/chat/completions", roleInferencePOST, true},
		{"lmstudio list", lmstudio, "GET", "/v1/models", roleModelListOpenAIGET, true},
		// LM Studio serves no native Ollama routes, so /api/chat is not
		// inference for it — it is forwarded verbatim like any other path.
		{"lmstudio has no native routes", lmstudio, "POST", "/api/chat", 0, false},

		// The method is part of the classification. Without it a POST to the
		// model-list path would be served as a list, and a GET to an
		// inference path would emit a workload.
		{"wrong method on list", ollama, "POST", "/v1/models", 0, false},
		{"wrong method on inference", ollama, "GET", "/api/chat", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role, ok := tc.profile.roleFor(tc.method, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("roleFor(%s %s) ok = %v, want %v", tc.method, tc.path, ok, tc.wantOK)
			}
			if ok && role != tc.wantRole {
				t.Fatalf("roleFor(%s %s) role = %v, want %v", tc.method, tc.path, role, tc.wantRole)
			}
		})
	}
}

func TestIsInferenceRequestFollowsTheProfile(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	if !isInferenceRequest(ollama, "POST", "/api/generate") {
		t.Error("ollama /api/generate must be inference")
	}
	if isInferenceRequest(lmstudio, "POST", "/api/generate") {
		t.Error("lmstudio serves no native routes, so /api/generate is not inference for it")
	}
	if isInferenceRequest(ollama, "GET", "/api/tags") {
		t.Error("a model list is not inference")
	}
}

// One naming convention governs both the federated list's dedupe key and the
// routing gate. If they disagreed the proxy could advertise a model it then
// refuses to route.
func TestModelNaming(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	for _, tc := range []struct {
		profile engineProfile
		in      string
		want    string
	}{
		{ollama, "llama3", "llama3:latest"},
		{ollama, "llama3:latest", "llama3:latest"},
		{ollama, "llama3:8b", "llama3:8b"},
		{ollama, "registry.example/llama3", "registry.example/llama3:latest"},
		{ollama, "llama3@sha256:abc", "llama3@sha256:abc"},
		{ollama, "", ""},

		{lmstudio, "qwen3-8b", "qwen3-8b"},
		{lmstudio, "qwen3-8b:latest", "qwen3-8b:latest"},
		{lmstudio, "", ""},
	} {
		if got := tc.profile.normalizeModel(tc.in); got != tc.want {
			t.Errorf("%s normalizeModel(%q) = %q, want %q", tc.profile.Name, tc.in, got, tc.want)
		}
	}
}

func TestNodeAdvertisesModelUsesTheProfilesNaming(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	tagged := Node{Models: []string{"llama3:latest"}}
	if !nodeAdvertisesModel(ollama, tagged, "llama3") {
		t.Error("ollama must treat an untagged request as the :latest tag")
	}
	if nodeAdvertisesModel(lmstudio, tagged, "llama3") {
		t.Error("lmstudio matches identifiers exactly, so llama3 is not llama3:latest")
	}
	if nodeAdvertisesModel(ollama, Node{Models: []string{"llama3:latest"}}, "") {
		t.Error("an empty request model advertises nothing")
	}
}

// A path may legitimately be declared twice under different methods. roleFor
// used to return on the first path match, which made the second entry
// unreachable — it would be forwarded verbatim instead of classified, and
// nothing would say so.
func TestRoleForFindsAPathDeclaredUnderTwoMethods(t *testing.T) {
	p := engineProfile{Routes: []route{
		{Path: "/v1/models", Role: roleInferencePOST},
		{Path: "/v1/models", Role: roleModelListOpenAIGET},
	}}

	if role, ok := p.roleFor("GET", "/v1/models"); !ok || role != roleModelListOpenAIGET {
		t.Errorf("GET /v1/models = (%v, %v), want the model-list role", role, ok)
	}
	if role, ok := p.roleFor("POST", "/v1/models"); !ok || role != roleInferencePOST {
		t.Errorf("POST /v1/models = (%v, %v), want the inference role", role, ok)
	}
	if _, ok := p.roleFor("DELETE", "/v1/models"); ok {
		t.Error("DELETE /v1/models classified; an undeclared method must forward verbatim")
	}
}

// No shipped engine declares the same (path, method) twice; a duplicate would
// make whichever entry came second dead.
func TestNoDuplicateRoutePerMethod(t *testing.T) {
	for _, p := range profiles {
		seen := map[string]bool{}
		for _, r := range p.Routes {
			key := r.Role.method() + " " + r.Path
			if seen[key] {
				t.Errorf("%s declares %q twice; the second entry is unreachable", p.Name, key)
			}
			seen[key] = true
		}
	}
}

// The error-ID prefix derives from Name so it cannot disagree with the
// broker's expectations. The persisted-port filename deliberately does NOT
// derive: Ollama's predates the multi-engine world, and deriving it would
// rename the file under every existing install and orphan the port a user set.
func TestDerivedIdentifiers(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")

	if got := ollama.PortFile; got != "proxy-port.json" {
		t.Errorf("ollama PortFile = %q, want the pre-unification name", got)
	}
	if got := lmstudio.PortFile; got != "lmstudio-proxy-port.json" {
		t.Errorf("lmstudio PortFile = %q", got)
	}
	if got := upstreamUnreachableID(ollama, "peer-A"); got != "ollama-proxy:upstream-unreachable:peer-A" {
		t.Errorf("ollama upstreamUnreachableID = %q", got)
	}
	if got := upstreamUnreachableID(lmstudio, "peer-A"); got != "lmstudio-proxy:upstream-unreachable:peer-A" {
		t.Errorf("lmstudio upstreamUnreachableID = %q", got)
	}
}

// chooseStartupPort restores a previously chosen port, except when the broker
// forces one or when the stored value belongs to the engine itself.
func TestChooseStartupPort(t *testing.T) {
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
			if got != tc.want {
				t.Fatalf("chooseStartupPort = %d, want %d", got, tc.want)
			}
		})
	}
}
