// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is about one question: when PAIR asks a port "are you <this engine>?",
// what counts as an answer?
//
// A status code alone cannot answer it. Three engines collide on /health on one
// Mac and each answers that endpoint differently, and none of the three says its
// own name. Captured from running servers, not read out of a README:
//
//	vLLM      -> 200, no body
//	llama.cpp -> 200, {"status":"ok"}
//	MTPLX     -> 200, {"ok":true, "model":..., "model_path":...}, 51,354 bytes
//
// So "got a 200 on /health" is not evidence about which engine answered, and PAIR
// treating it as such is what reported a relocated MTPLX as a running vLLM. The
// rule the tests below hold is the narrow one that fixes that: a port may still
// answer "this is my engine" when its body proves nothing, because that is what a
// real vLLM answers; it may not answer that when its body proves somebody else is
// there, either by carrying a key another engine declares as its identity or by
// being too large for PAIR to have read at all.
//
// The MTPLX fixtures are deliberately not smaller than the real body. A matcher
// tested only against a 30-byte stub has not been tested, because the real body is
// 51 KB and its identifying keys sit inside that.

const (
	// mtplxServedModel and mtplxServedPath are the name and folder one real
	// MTPLX reports for the pack it was launched with.
	mtplxServedModel = "mtplx-flash-next-optimized-speed"
	mtplxServedPath  = "/Users/Shared/mtplx/models/Youssofal--Qwen3.8-Flash-Next-MTPLX-Optimized-Speed"

	// realMTPLXHealthBytes is the size of the /health body captured from a
	// running MTPLX. A fixture smaller than this would let a truncated read pass.
	realMTPLXHealthBytes = 51354

	// vllmHealthBody is vLLM's whole /health answer: a 200 and nothing else.
	vllmHealthBody = ""

	// llamaCPPHealthBody is llama.cpp's. It has no key in common with MTPLX's,
	// which is why the two need different identity declarations.
	llamaCPPHealthBody = `{"status":"ok"}`
)

// hostPlatformKey is the key every manifest indexes its platform blocks under.
func hostPlatformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

// mtplxHealthBody builds MTPLX's /health body: the identifying keys in the places
// the real one puts them, then enough filler to reach the size the real one is.
// The filler lives under its own key so it cannot be mistaken for a second copy of
// an identifying key.
func mtplxHealthBody(t *testing.T, model, modelPath string) string {
	t.Helper()

	// The real body reports per-kernel dispatch ledgers. Only their size matters
	// to these tests: they are what pushes the response past every assumption a
	// reader might make about a health check being short.
	filler := make(map[string]any, 2600)
	for i := range 2600 {
		filler[fmt.Sprintf("sdpa_nax_flash_%04d", i)] = []any{
			"engaged", i, "cache_bank", "resident",
			fmt.Sprintf("kernel_selfcheck_line_%d", i),
		}
	}
	body, err := json.Marshal(map[string]any{
		"ok":                true,
		"model":             model,
		"model_path":        modelPath,
		"machine_model":     "Mac16,7",
		"generation_mode":   "mtp",
		"api_key_required":  false,
		"kernel_ledger":     filler,
		"available_modes":   []string{"mtp", "ar"},
		"idle_seconds":      0.13,
		"active_requests":   0,
		"foreground_active": 0,
	})
	require.NoError(t, err, "the health fixture must be constructible")
	require.GreaterOrEqual(t, len(body), realMTPLXHealthBytes,
		"the fixture must be at least as large as the real %d-byte body, or a truncated read passes for a full one",
		realMTPLXHealthBytes)
	return string(body)
}

// servesBody answers `status` with `body` on every path, which is how a foreign
// server behaves toward a probe that asks it the wrong question.
func servesBody(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if body == "" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// probeOnlyEngine builds a manifest whose only claim about the world is the
// identity question its probe asks. The other engines have to be in the registry
// for these tests because that is where a probe learns what "somebody else" looks
// like: the real registry holds every manifest, so a probe that yields to foreign
// evidence has somewhere to find it, and no engine has to be hand-named by another
// engine's manifest.
func probeOnlyEngine(name string, ready *Probe) *Manifest {
	return &Manifest{
		Engine: name, DisplayName: strings.ToUpper(name), ManifestVersion: 1,
		Platforms: map[string]Platform{
			hostPlatformKey(): {Runtime: Runtime{Ready: ready}},
		},
	}
}

// executorAsking builds an executor over several engines at once, which is the
// only way to test what one engine's probe concludes about another's server.
func executorAsking(t *testing.T, manifests ...*Manifest) *Executor {
	t.Helper()
	reg := NewRegistry()
	for _, m := range manifests {
		reg.engines[m.Engine] = m
	}
	return NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
}

// healthProbe is the shape every one of the colliding engines shares: a GET on
// /health satisfied by any 200.
func healthProbe() *Probe {
	return &Probe{HTTP: "http://127.0.0.1:{port}/health", Status: 200, TimeoutS: 60}
}

// healthProbeAskingForMTPLXsKey is what MTPLX's manifest must declare once a
// probe can express a value that is not a string: /health answers, and it answers
// with MTPLX's own key. This is the same test MTPLX applies to itself — its own
// client counts a port as one of MTPLX's only when /health answers "ok": true.
func healthProbeAskingForMTPLXsKey() *Probe {
	return &Probe{
		HTTP:      "http://127.0.0.1:{port}/health",
		Status:    200,
		TimeoutS:  60,
		JSONMatch: &ProbeJSONMatch{Field: "ok", Value: "true"},
	}
}

// A relocated MTPLX landing on vLLM's probe port is how a Mac with no vLLM came to
// report "vllm running:true port:8001". The status code matched, so PAIR believed
// it, and from there PAIR sent that router a model name it does not serve and got
// confident wrong answers. Every body here is one a real server gives back.
func TestProbeIdentity_VLLMRejectsAHealthAnswerThatNamesAnotherEngine(t *testing.T) {
	const oversizedNote = "a body larger than PAIR's read limit was read as vLLM anyway"

	tests := []struct {
		name string
		// foreign is an extra engine whose declared identity the body answers.
		foreign *Manifest
		body    func(t *testing.T) string
		why     string
	}{
		{
			name:    "the whole MTPLX health body",
			foreign: probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()),
			body: func(t *testing.T) string {
				return mtplxHealthBody(t, mtplxServedModel, mtplxServedPath)
			},
			why: "MTPLX answers /health with its own keys and no vLLM does",
		},
		{
			// Not a fixture from the field: a made-up engine, to pin where the
			// knowledge comes from. If the rule were hand-written per manifest,
			// this body would read as vLLM and only MTPLX would be caught.
			name:    "a made-up engine's own identity key",
			foreign: probeOnlyEngine("otherfake", &Probe{HTTP: "http://127.0.0.1:{port}/health", JSONMatch: &ProbeJSONMatch{Field: "mystery_ready", Value: "true"}}),
			body: func(*testing.T) string {
				return `{"mystery_ready":true,"note":"some service of somebody else's"}`
			},
			why: "a body answering another manifest's declared identity is not this engine, whoever wrote that manifest",
		},
		{
			name:    "a web app's HTML, larger than the read limit",
			foreign: probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()),
			body: func(*testing.T) string {
				// A dev server with history-api fallback answers every path,
				// including /health, with its app shell.
				return "<!doctype html><html><body>" + strings.Repeat(`<div id="root">app shell</div>`, 40_000) + "</body></html>"
			},
			why: oversizedNote + ", and an unreadable body proves nobody",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			srv := servesBody(t, http.StatusOK, body)
			probe := healthProbe()
			ex := executorAsking(t, probeOnlyEngine("vllm", probe), tc.foreign)

			answer := ex.probeIdentity(t.Context(), probe, portOf(t, srv.URL))

			assert.NotEqual(t, probeIdentified, answer.outcome,
				"%s, yet the port was accepted as vLLM (%+v)", tc.why, answer)
			assert.False(t, answer.blocked(),
				"a stranger is not a credential refusal, so it must not be reported as one (%+v)", answer)
		})
	}
}

// The other half of the same rule, and the reason it is not simply "distrust every
// body": a real vLLM's /health has no body to identify with, and a real llama.cpp
// answers a key that belongs to neither. Both must still be detectable, or the fix
// for one mis-read becomes a new way to lose a running engine.
func TestProbeIdentity_VLLMStillAcceptsAHealthAnswerThatProvesNothing(t *testing.T) {
	tests := []struct {
		name string
		body string
		why  string
	}{
		{name: "vLLM answers nothing", body: vllmHealthBody, why: "an empty 200 is what vLLM really returns"},
		{name: "a service answers a key nobody claims", body: `{"models":[]}`, why: "a key no manifest declares as an identity proves nothing either way"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := servesBody(t, http.StatusOK, tc.body)
			probe := healthProbe()
			ex := executorAsking(t,
				probeOnlyEngine("vllm", probe),
				probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()),
				probeOnlyEngine("llamacpp", &Probe{HTTP: "http://127.0.0.1:{port}/props", JSONMatch: &ProbeJSONMatch{Field: "role", Value: "router"}}),
			)

			answer := ex.probeIdentity(t.Context(), probe, portOf(t, srv.URL))

			require.Equal(t, probeIdentified, answer.outcome,
				"%s, and this machine really runs one: refusing it would report a working engine as offline", tc.why)
		})
	}
}

// Read from the other engine: MTPLX's manifest asks only for a 200 today, so it
// accepts vLLM's empty body and llama.cpp's {"status":"ok"} as proof of MTPLX. The
// mistake runs both ways until each engine is asked about a key it owns.
func TestProbeIdentity_MTPLXRequiresItsOwnKey(t *testing.T) {
	tests := []struct {
		name string
		body string
		why  string
	}{
		{name: "vLLM answers nothing", body: vllmHealthBody, why: "vLLM's /health is an empty 200"},
		{name: "llama.cpp answers its own key", body: llamaCPPHealthBody, why: "llama.cpp reports status, not ok"},
		{name: "a stranger answers an empty object", body: `{}`, why: "a 200 with no keys proves nothing"},
		{name: "the keys are there with the wrong values", body: `{"ok":false,"model_path":""}`,
			why: "a false ok and an absent model folder are not a running MTPLX"},
		{
			name: "a model folder without MTPLX's liveness key",
			body: `{"model":"someone-elses-model","model_path":"/opt/someone/models/thing"}`,
			why:  "a path key is shared with other routers, so it cannot be how MTPLX is recognised",
		},
	}

	probe := healthProbeAskingForMTPLXsKey()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := servesBody(t, http.StatusOK, tc.body)
			ex := executorAsking(t,
				probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()),
				probeOnlyEngine("vllm", healthProbe()),
			)

			answer := ex.probeIdentity(t.Context(), probe, portOf(t, srv.URL))

			assert.NotEqual(t, probeIdentified, answer.outcome,
				"%s, yet the port was accepted as MTPLX (%+v)", tc.why, answer)
		})
	}
}

// A model-less MTPLX is still MTPLX and still must not be duplicated: the server
// fills model and model_path from its launch arguments, so they say what it was
// pointed at, not whether it is here. A user who starts MTPLX with no pack has one
// router; PAIR adopting it keeps that true.
func TestProbeIdentity_MTPLXRequiresItsOwnKey_ModellessMTPLXStillCounts(t *testing.T) {
	srv := servesBody(t, http.StatusOK, `{"ok":true,"model":"","model_path":"","api_key_required":false}`)
	ex := executorAsking(t, probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()))

	answer := ex.probeIdentity(t.Context(), healthProbeAskingForMTPLXsKey(), portOf(t, srv.URL))

	require.Equal(t, probeIdentified, answer.outcome,
		"a MTPLX with no pack loaded was not recognised as MTPLX, so PAIR would start a second one beside it")
}

// The requirement only means something if the real body satisfies it, at its real
// size and with its real keys.
func TestProbeIdentity_MTPLXRequiresItsOwnKey_TheRealHealthBodyPasses(t *testing.T) {
	srv := servesBody(t, http.StatusOK, mtplxHealthBody(t, mtplxServedModel, mtplxServedPath))
	ex := executorAsking(t, probeOnlyEngine("mtplx", healthProbeAskingForMTPLXsKey()))

	answer := ex.probeIdentity(t.Context(), healthProbeAskingForMTPLXsKey(), portOf(t, srv.URL))

	require.Equal(t, probeIdentified, answer.outcome,
		"a real MTPLX health body did not read as MTPLX, so a user's own engine would be invisible to PAIR")
}

// End to end, the shape seen on two Macs: no vLLM installed anywhere, a user's
// MTPLX relocated onto the port PAIR reserves for probing vLLM, and both engines'
// real manifests loaded. A bare 200 on that port used to read as a vLLM that was
// installed, running and healthy, which is how a chat kept getting confident answers
// from an engine the user never chose.
func TestStatus_VLLM_ARelocatedMTPLXOnItsProbePortIsNotARunningVLLM(t *testing.T) {
	srv := servesBody(t, http.StatusOK, mtplxHealthBody(t, mtplxServedModel, mtplxServedPath))
	servedPort := portOf(t, srv.URL)

	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	require.Contains(t, reg.Names(), "mtplx",
		"MTPLX must be in the registry for its declared identity to be readable there")

	vllm, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")
	key := hostPlatformKey()
	plat, ok := vllm.Platforms[key]
	if !ok {
		// vLLM does not run on every platform, and where it cannot be installed
		// there is no vLLM card to mis-read.
		t.Skipf("vLLM has no block for %s, so no relocated MTPLX can land on its probe port here", key)
	}
	require.NotNil(t, plat.Runtime.Ready, "the collision happens on vLLM's readiness probe")

	// No vLLM installed on this Mac: the mis-read did not depend on one. A test
	// server stands in for MTPLX on vLLM's probe port, which is where MTPLX's
	// own relocation rule puts it after PAIR's facade takes 8000.
	plat.Detect = nil
	plat.Runtime.Bin = ""
	plat.Runtime.Port = servedPort
	vllm.Platforms[key] = plat

	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())

	st, err := ex.Status("vllm")

	require.NoError(t, err)
	assert.False(t, st.Running, "a relocated MTPLX was reported as a running vLLM (%v)", st)
	assert.False(t, st.Healthy, "a relocated MTPLX was reported as a healthy vLLM (%v)", st)
	assert.False(t, st.Installed, "a relocated MTPLX made vLLM look installed (%v)", st)
}

// The shipped manifest is where the identity answer has to live, so the manifest is
// what gets pinned. Two things must hold and only one of them is visible by reading
// the JSON: the key it declares has to be the key a real MTPLX sends, at its real
// size, and it has to refuse the routers that share /health. Every platform block is
// checked, so a manifest for another architecture cannot ship the loose probe, and
// the probes are read back from the loaded registry, so it is the edited JSON that is
// under test rather than a copy of it.
func TestMTPLXManifest_DeclaresItsOwnIdentityOnBothProbes(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	m, ok := reg.Get("mtplx")
	require.True(t, ok, "mtplx manifest not loaded")

	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())

	// What the three routers that collide on /health really answer.
	answers := []struct {
		who     string
		body    string
		isMTPLX bool
	}{
		{who: "a running MTPLX", body: mtplxHealthBody(t, mtplxServedModel, mtplxServedPath), isMTPLX: true},
		{who: "a MTPLX with no pack loaded", body: `{"ok":true,"model":"","model_path":"","api_key_required":false}`, isMTPLX: true},
		{who: "vLLM", body: vllmHealthBody, isMTPLX: false},
		{who: "llama.cpp", body: llamaCPPHealthBody, isMTPLX: false},
	}

	for key, plat := range m.Platforms {
		for which, probe := range map[string]*Probe{"ready": plat.Runtime.Ready, "health": plat.Runtime.Health} {
			require.NotNil(t, probe, "%s: runtime.%s is missing", key, which)
			match := probe.JSONMatch
			require.NotNil(t, match,
				"%s: runtime.%s is satisfied by any 200, so any router answering /health reads as MTPLX",
				key, which)
			assert.Equal(t, "ok", match.Field, "%s: runtime.%s asks about the wrong key", key, which)
			assert.Equal(t, "true", match.Value, "%s: runtime.%s expects the wrong value", key, which)

			for _, answer := range answers {
				t.Run(key+"/"+which+"/"+answer.who, func(t *testing.T) {
					srv := servesBody(t, http.StatusOK, answer.body)
					got := ex.probe(t.Context(), probe, portOf(t, srv.URL))
					if answer.isMTPLX {
						require.True(t, got,
							"%s is a MTPLX, and a manifest that cannot see it leaves the user's own router invisible to be started a second time beside",
							answer.who)
						return
					}
					assert.False(t, got, "%s is not MTPLX, yet the manifest accepted its health answer", answer.who)
				})
			}
		}
	}
}
