// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Prober contract for Custom endpoints (Task 5).
//
// One GET of the endpoint's model-list path per probe cycle, Authorization
// injected only when the endpoint carries a key, and change-gated status
// events to the broker that never carry the key. Health follows the
// manual-node failure style: a healthy endpoint keeps its routability
// through blips and reads unhealthy only at probeFailThreshold consecutive
// misses; a recovering endpoint is healthy again on the first success.
//
// The endpoint-level test harness is the same fakeRoundTripper the manual-node
// tests use, so a probe's outgoing request (its Authorization header included)
// is directly assertable.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const endpointSecret = "sk-custom-probe-secret"

// plainEndpoint is a scheme+host+port base with no path, the common case for
// a self-hosted OpenAI-compatible server.
const plainEndpoint = "http://endpoint.test:8000"

func customProbeKey() string {
	return "custom:" + plainEndpoint
}

// seedEndpoint plants a tracked endpoint mid-cycle (mirrors how the
// manual-node tests seed m.nodes) so failure-threshold and recovery paths can
// be driven probe-by-probe without replaying whole cadences.
func seedEndpoint(t *testing.T, m *Manager, baseURL, apiKey string, healthy bool, models []string) *trackedCustom {
	t.Helper()
	tc := &trackedCustom{
		entry:   CustomEntry{BaseURL: baseURL, APIKey: apiKey},
		healthy: healthy,
		models:  models,
	}
	m.mu.Lock()
	m.customs[tc.entry.key()] = tc
	m.mu.Unlock()
	return tc
}

// awaitHeader waits for the probe's outgoing Authorization header. The empty
// string is a real observation ("no header sent"), so it is delivered through
// the same channel as a present value.
func awaitHeader(t *testing.T, seen chan string) string {
	t.Helper()
	select {
	case auth := <-seen:
		return auth
	case <-time.After(2 * time.Second):
		t.Fatal("probe never reached the endpoint")
		return ""
	}
}

func TestCustomProbe_KeyGatedUpstream_ReceivesBearerHeaderAndReportsModels(t *testing.T) {
	m, rw, rt := newTestManager()
	seenAuth := make(chan string, 1)
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(req *http.Request) (*http.Response, error) {
		seenAuth <- req.Header.Get("Authorization")
		return httpJSON(http.StatusOK, `{"data":[{"id":"mtplx-7b"},{"id":"vllm-32b"}]}`)
	})

	m.handleMessage(requestMessage(t, 1, "custom/add", CustomEntry{BaseURL: plainEndpoint, APIKey: endpointSecret}))

	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	initial := decodeResult[CustomEndpointStatus](t, resp)
	assert.Equal(t, customProbeKey(), initial.ID, "derived endpoint id")
	assert.False(t, initial.Healthy, "the add response is the unprobed state")

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	require.True(t, status.Healthy, "probe result did not report healthy (%v)", status)
	assert.Equal(t, []string{"mtplx-7b", "vllm-32b"}, status.Models, "models")
	assert.NotContains(t, string(event.Params), endpointSecret, "status event carried the endpoint key")

	assert.Equal(t, "Bearer "+endpointSecret, awaitHeader(t, seenAuth),
		"Authorization sent to the endpoint (OpenAI convention)")
}

func TestCustomProbe_KeylessEndpoint_SendsNoAuthorizationHeader(t *testing.T) {
	m, rw, rt := newTestManager()
	seenAuth := make(chan string, 1)
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(req *http.Request) (*http.Response, error) {
		auth, ok := req.Header["Authorization"]
		if ok {
			seenAuth <- auth[0]
		} else {
			seenAuth <- ""
		}
		return httpJSON(http.StatusOK, `{"data":[{"id":"m1"}]}`)
	})

	m.handleMessage(requestMessage(t, 1, "custom/add", CustomEntry{BaseURL: plainEndpoint}))

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	assert.True(t, decodeParams[CustomEndpointStatus](t, event).Healthy, "keyless probe")

	assert.Empty(t, awaitHeader(t, seenAuth), "keyless probe must send no Authorization header")
}

func TestCustomProbe_KeepsHealthyThroughBlipsUntilThreshold(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusUnauthorized, `{"error":"missing key"}`)
	})
	tc := seedEndpoint(t, m, plainEndpoint, "", true, []string{"mtplx-7b"})

	// The first two misses change no state, so nothing fires: the endpoint
	// stays a routing candidate while its failure count is below threshold.
	m.probeCustom(tc)
	m.probeCustom(tc)
	assertNoCaptureMethod(t, rw, "custom/updated")

	// The third consecutive miss flips health, and the unhealthy status
	// drops the stale model list (an empty inventory is not routable).
	m.probeCustom(tc)
	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.False(t, status.Healthy, "3 consecutive 401s did not mark the endpoint unhealthy")
	require.Empty(t, status.Models, "unhealthy status retained stale models (%v)", status)
}

func TestCustomProbe_ReportsRecoveryOnFirstSuccessAfterFailures(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusUnauthorized, `{}`)
	})
	tc := seedEndpoint(t, m, plainEndpoint, "", true, []string{"mtplx-7b"})

	// Fall: three misses flip health down, and the flip event is consumed.
	m.probeCustom(tc)
	m.probeCustom(tc)
	m.probeCustom(tc)
	_ = readCaptureUntil(t, rw, methodIs("custom/updated"))

	// Recover: one success is enough — recovery is never threshold-gated.
	// The model list returns with the health, replacing the cleared list.
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"mtplx-7b"}]}`)
	})
	m.probeCustom(tc)

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.True(t, status.Healthy, "recovery did not flip health back")
	assert.Equal(t, []string{"mtplx-7b"}, status.Models, "recovered models")
}

func TestCustomProbe_EmptyModelListIsHealthyWithNoModels(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"object":"list","data":[]}`)
	})
	tc := seedEndpoint(t, m, plainEndpoint, "", false, nil)

	// A valid-but-empty list answers the probe: healthy (the endpoint is
	// configured and answering) but not yet routable — that second judgement
	// belongs to the discovery layer, which is why health and models are
	// reported separately.
	m.probeCustom(tc)

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.True(t, status.Healthy, "empty-but-valid list should read healthy (%v)", status)
	require.Empty(t, status.Models, "empty list should report no models (%v)", status)
}

func TestCustomProbe_ParseFailureDropsAHealthyEndpoint(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `this is not the OpenAI envelope`)
	})
	tc := seedEndpoint(t, m, plainEndpoint, "", true, []string{"mtplx-7b"})

	// Unlike the LM Studio probe (where "reachable but unparseable" stays
	// up with no models), a Custom endpoint that never answers with a
	// parseable model list is unhealthy once the consecutive-miss threshold
	// is crossed: a wrong-format answer is a wrong endpoint.
	m.probeCustom(tc)
	m.probeCustom(tc)
	m.probeCustom(tc)

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.False(t, status.Healthy, "unparseable model list did not mark the endpoint unhealthy")
	require.Empty(t, status.Models, "unhealthy status retained stale models (%v)", status)
}

func TestCustomProbe_ModelSetChangeTriggersStatusEvent(t *testing.T) {
	m, rw, rt := newTestManager()
	tc := seedEndpoint(t, m, plainEndpoint, "", true, []string{"mtplx-7b"})

	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"vllm-32b"}]}`)
	})
	m.probeCustom(tc)

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.True(t, status.Healthy, "model-set change kept health")
	assert.Equal(t, []string{"vllm-32b"}, status.Models, "changed model set")
}

func TestCustomProbeURL_JoinsTheModelListPath(t *testing.T) {
	for _, tc := range []struct{ name, baseURL, want string }{
		{"bare origin", "http://10.9.8.7:8000", "http://10.9.8.7:8000/v1/models"},
		{"base already ends at /v1", "http://10.9.8.7:8000/v1", "http://10.9.8.7:8000/v1/models"},
		{"trailing slash", "http://10.9.8.7:8000/", "http://10.9.8.7:8000/v1/models"},
		{"deployment path", "https://relay.example/relays", "https://relay.example/relays/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, customProbeURL(tc.baseURL))
		})
	}
}

func TestCustomEntryKey_PrefersExplicitIDOverBaseURLFallback(t *testing.T) {
	assert.Equal(t, "gpu-box", CustomEntry{ID: "gpu-box", BaseURL: plainEndpoint}.key(), "explicit id wins")
	assert.Equal(t, customProbeKey(), CustomEntry{BaseURL: plainEndpoint}.key(), "baseUrl fallback")
}

func TestCustomEntry_DecodesTheBrokerConfigWireShape(t *testing.T) {
	// Pins the cross-process contract: nvpair-ui-broker forwards its
	// persistedCustomEndpoint JSON verbatim (live add and replay after a
	// prober respawn), so CustomEntry must decode exactly that field
	// spelling. A drift here loses fields silently at the boundary —
	// apiKey most of all.
	var entry CustomEntry
	require.NoError(t, json.Unmarshal([]byte(`{"id":"gpu-box","name":"Garage Box","baseUrl":"http://endpoint.test:8000","apiKey":"sk-custom-probe-secret"}`), &entry))
	assert.Equal(t, "gpu-box", entry.ID, "id")
	assert.Equal(t, "sk-custom-probe-secret", entry.APIKey, "apiKey")
	assert.Equal(t, "gpu-box", entry.key(), "explicit id beats baseUrl")

	var replay CustomEntry
	require.NoError(t, json.Unmarshal([]byte(`{"baseUrl":"http://endpoint.test:8000"}`), &replay))
	assert.Equal(t, customProbeKey(), replay.key(), "replay without id derives the key from baseUrl")
}

func TestCustomRemove_DropsEntryAndNotifiesRemoval(t *testing.T) {
	m, rw, _ := newTestManager()
	seedEndpoint(t, m, plainEndpoint, endpointSecret, true, []string{"mtplx-7b"})

	m.handleMessage(requestMessage(t, 1, "custom/remove", map[string]string{"id": customProbeKey()}))

	removed := readCaptureFrame(t, rw)
	assert.Equal(t, "custom/removed", removed.Method, "removal notification frame (%v)", removed)
	params := decodeParams[CustomEndpointStatus](t, removed)
	assert.Equal(t, customProbeKey(), params.ID, "removed params")
	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	result := decodeResult[map[string]bool](t, resp)
	assert.True(t, result["removed"], "remove response (%v)", result)

	m.mu.RLock()
	require.Empty(t, m.customs, "endpoint still tracked after removal")
	m.mu.RUnlock()

	m.handleMessage(requestMessage(t, 2, "custom/remove", map[string]string{"id": customProbeKey()}))
	resp = readCaptureUntil(t, rw, responseWithID(t, 2))
	result = decodeResult[map[string]bool](t, resp)
	assert.False(t, result["removed"], "second removal result (%v)", result)
	assertNoCaptureMethod(t, rw, "custom/removed")
}

func TestCustomAdd_ReAddingSameEndpointReplacesAndReprobes(t *testing.T) {
	m, rw, rt := newTestManager()
	seedEndpoint(t, m, plainEndpoint, "", true, []string{"old-model"})
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"new-model"}]}`)
	})

	// The broker's edit flow is re-add-with-the-same-key (there is no
	// on/off state to toggle), so the prober must replace the tracked entry
	// in place rather than keep the stale probe state under the same key.
	m.handleMessage(requestMessage(t, 1, "custom/add", CustomEntry{BaseURL: plainEndpoint}))

	event := readCaptureUntil(t, rw, methodIs("custom/updated"))
	status := decodeParams[CustomEndpointStatus](t, event)
	assert.True(t, status.Healthy, "replaced endpoint not probed healthy (%v)", status)
	assert.Equal(t, []string{"new-model"}, status.Models, "replaced endpoint kept the stale model set")

	m.mu.RLock()
	require.Len(t, m.customs, 1, "re-add duplicated the entry")
	m.mu.RUnlock()
}

// probeReply mirrors the custom/probe response body: a snapshot of the
// addressed scope taken after a fresh probe cycle, never null.
type probeReply struct {
	Endpoints []CustomEndpointStatus `json:"endpoints"`
}

func TestProbeCustom_ReplacedEntryStartsUnprobed(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"old-model"}]}`)
	})
	seedEndpoint(t, m, plainEndpoint, "", true, []string{"old-model"})

	// A replacement carries a new model set. The scoped probe must answer
	// with the *new* endpoint's truth: a tracked entry's health and model
	// list belong to whatever endpoint produced them, so a mid-flight or
	// stale snapshot of the old endpoint must never surface.
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"new-model"}]}`)
	})
	m.handleMessage(requestMessage(t, 1, "custom/add", CustomEntry{BaseURL: plainEndpoint}))
	m.handleMessage(requestMessage(t, 2, "custom/probe", map[string]string{"id": customProbeKey()}))

	resp := readCaptureUntil(t, rw, responseWithID(t, 2))
	reply := decodeResult[probeReply](t, resp)
	require.Len(t, reply.Endpoints, 1, "scoped probe reply (%v)", reply)
	assert.True(t, reply.Endpoints[0].Healthy, "probe answered before the fresh cycle finished")
	assert.Equal(t, []string{"new-model"}, reply.Endpoints[0].Models, "stale model set leaked into the reply")
}

func TestHandleMessage_CustomProbeRequest_ScopedResponse(t *testing.T) {
	m, rw, rt := newTestManager()
	seenB := make(chan string, 4)
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"fresh-a"}]}`)
	})
	rt.set(http.MethodGet, "other.test:9000", "/v1/models", func(*http.Request) (*http.Response, error) {
		seenB <- "probed"
		return httpJSON(http.StatusOK, `{"data":[{"id":"fresh-b"}]}`)
	})
	seedEndpoint(t, m, plainEndpoint, "", true, []string{"seed-a"})
	seedEndpoint(t, m, "http://other.test:9000", "", true, []string{"seed-b"})

	m.handleMessage(requestMessage(t, 1, "custom/probe", map[string]string{"id": customProbeKey()}))

	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	reply := decodeResult[probeReply](t, resp)
	require.Len(t, reply.Endpoints, 1, "an addressed probe answers only for that endpoint (%v)", reply)
	assert.Equal(t, customProbeKey(), reply.Endpoints[0].ID, "addressed endpoint id")
	assert.True(t, reply.Endpoints[0].Healthy, "healthy")
	assert.Equal(t, []string{"fresh-a"}, reply.Endpoints[0].Models, "fresh model set, not the seed")

	select {
	case got := <-seenB:
		t.Fatalf("the unaddressed endpoint was probed (%s)", got)
	default:
	}
}

func TestHandleMessage_CustomProbeRequest_EmptyParamsProbeEveryEndpoint(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"fresh-a"}]}`)
	})
	rt.set(http.MethodGet, "other.test:9000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"fresh-b"}]}`)
	})
	seedEndpoint(t, m, plainEndpoint, "", true, []string{"seed-a"})
	seedEndpoint(t, m, "http://other.test:9000", "", true, []string{"seed-b"})

	m.handleMessage(requestMessage(t, 1, "custom/probe", struct{}{}))

	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	reply := decodeResult[probeReply](t, resp)
	require.Len(t, reply.Endpoints, 2, "unaddressed probe covers every tracked endpoint (%v)", reply)
}

func TestHandleMessage_CustomProbeRequest_UnknownIdAnswersEmptyList(t *testing.T) {
	m, rw, rt := newTestManager()
	rt.set(http.MethodGet, "endpoint.test:8000", "/v1/models", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"data":[{"id":"fresh-a"}]}`)
	})
	seedEndpoint(t, m, plainEndpoint, "", true, []string{"seed-a"})

	m.handleMessage(requestMessage(t, 1, "custom/probe", map[string]string{"id": "custom:http://nowhere.test:1"}))

	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	reply := decodeResult[probeReply](t, resp)
	require.NotNil(t, reply.Endpoints, "the snapshot is an array, never null")
	assert.Empty(t, reply.Endpoints, "a probe for an unknown endpoint answers empty")
}

func TestCustomAdd_RejectsInvalidParameters(t *testing.T) {
	m, rw, _ := newTestManager()

	test := func(name string, request *Message) {
		t.Run(name, func(t *testing.T) {
			m.handleMessage(request)
			resp := readCaptureFrame(t, rw)
			require.NotNil(t, resp.Error, "invalid add should be rejected")
			assert.Equal(t, -32602, resp.Error.Code)
		})
	}
	test("malformed params", requestMessageRaw(t, 1, "custom/add", json.RawMessage(`"bad"`)))
	test("missing baseUrl", requestMessage(t, 2, "custom/add", CustomEntry{APIKey: endpointSecret}))

	m.mu.RLock()
	require.Empty(t, m.customs, "validation errors added endpoints")
	m.mu.RUnlock()
}
