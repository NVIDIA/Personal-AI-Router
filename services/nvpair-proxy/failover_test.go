// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rwNop is a no-op io.ReadWriter so a Codec can be constructed in tests
// without a real transport: reads hit EOF immediately and writes are
// discarded. handleHTTP only ever writes (notifications), so this is enough.
type rwNop struct{}

func (rwNop) Read([]byte) (int, error)    { return 0, io.EOF }
func (rwNop) Write(p []byte) (int, error) { return len(p), nil }

// newTestProxy builds a proxy with one facade attached but not started, which
// is what nearly every test wants: routing, dispatch, and handler behavior
// without binding a port.
//
// Production brings a facade up through facade/enable, which also binds,
// announces ready, and subscribes for routing targets. A test that needs any of
// that calls facade.start itself — see the alias and port-store tests.
func newTestProxy(profile engineProfile, codec *Codec, disc *Discovery, port int) *Proxy {
	p := NewProxy(codec)
	p.facades = map[string]*facade{
		profile.Name: newFacade(p, profile, disc, port),
	}
	return p
}

// soleFacade returns the one facade a single-engine test proxy was built with.
//
// Production resolves facades by engine, because a process hosts several. A
// test that builds exactly one has nothing to disambiguate, so naming the
// engine at every call site would be noise; tests that do host two address them
// explicitly instead.
func (p *Proxy) soleFacade() *facade {
	p.facadeMu.Lock()
	defer p.facadeMu.Unlock()
	if len(p.facades) != 1 {
		panic(fmt.Sprintf("soleFacade: proxy has %d facades, want exactly 1", len(p.facades)))
	}
	for _, f := range p.facades {
		return f
	}
	return nil
}

func testProxy(p engineProfile, disc *Discovery, port int) *Proxy {
	return newTestProxy(p, NewCodec(rwNop{}), disc, port)
}

// nodeFor turns an httptest server URL into a discovery Node pointing at it.
func nodeFor(t *testing.T, id, serverURL string) Node {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse %q: %v", serverURL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return Node{ID: id, Addresses: []string{host}, Port: port}
}

func nodeForModel(t *testing.T, id, serverURL, model string) Node {
	t.Helper()
	node := nodeFor(t, id, serverURL)
	node.Models = []string{model}
	return node
}

// Preflight cannot authorize a browser when no engine is reachable.
func TestHandlePlain_PreflightWithoutEngineFails(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		p := testProxy(tc.profile, NewDiscovery(), tc.profile.FacadePort)
		req := corsRequest(http.MethodOptions, tc.inferencePath, "http://app.test")
		rec := httptest.NewRecorder()
		p.soleFacade().handlePlain(rec, req)
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
		}
	})
}

// TestHandlePlain_EngineCredentialedPreflightPreserved: when an engine opts an
// exact origin into credentialed CORS, its preflight policy reaches the browser
// instead of being replaced by the proxy's uncredentialed wildcard fallback.
func TestHandlePlain_EngineCredentialedPreflightPreserved(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		preflightSeen := make(chan struct{}, 1)
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodOptions {
				t.Errorf("engine method = %s, want OPTIONS", r.Method)
			}
			preflightSeen <- struct{}{}
			w.Header().Set("Access-Control-Allow-Origin", "https://app.example")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
		}))
		defer engine.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeFor(t, "engine", engine.URL))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		req := httptest.NewRequest(http.MethodOptions, tc.inferencePath, nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("Origin", "https://app.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "Content-Type")
		rec := httptest.NewRecorder()

		p.soleFacade().handlePlain(rec, req)

		select {
		case <-preflightSeen:
		default:
			t.Fatal("engine did not receive the credentialed preflight")
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the engine's exact origin", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want the engine's true", got)
		}
	})
}

// TestHandleHTTP_EngineCORSPolicyPreserved: an engine that declares its own
// origin policy keeps it. Replacing it with the proxy's wildcard would widen
// what the user configured, and would break a credentialed response outright.
func TestHandleHTTP_EngineCORSPolicyPreserved(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "https://app.example")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer engine.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "engine", engine.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the engine's own origin", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want the engine's true", got)
		}
	})
}

// Preserve incomplete upstream policy without adding permissions.
func TestHandleHTTP_EngineCredentialsWithoutOriginPreserved(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer engine.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "engine", engine.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Access-Control-Allow-Credentials = %q, want upstream value preserved", got)
		}
	})
}

// TestHandleHTTP_HappyPathSingleNode: the common case — one healthy node
// answers directly, preserving the body and absence of CORS permissions.
func TestHandleHTTP_HappyPathSingleNode(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		var gotBody string
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer good.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "good", good.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if gotBody != tc.inferenceBody() {
			t.Errorf("node got body %q, want the original request body", gotBody)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header on success", got)
		}
	})
}

// TestHandleHTTP_NoRetryOn400: a client error (400) is returned as-is and not
// failed over — retrying elsewhere would return the same error and mask it.
func TestHandleHTTP_NoRetryOn400(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		hits := 0
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"bad request"}`)
		}))
		defer bad.Close()
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer other.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "bad", bad.URL, tc.advertisedModel))
		disc.AddManual(nodeForModel(t, "other", other.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("bad")

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (client errors must not fail over)", rec.Code)
		}
		if hits != 1 {
			t.Errorf("bad node hit %d times, want exactly 1 (no retry on 400)", hits)
		}
	})
}

// Proxy-generated errors grant no cross-origin access.
func TestHandleHTTP_RejectionHasNoCORS(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		p := testProxy(tc.profile, NewDiscovery(), tc.profile.FacadePort)
		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header on rejection", got)
		}
	})
}

// TestHandleHTTP_FailoverOn503: a busy first node (503) is skipped and the
// request is filled by the next node, with the original body replayed.
func TestHandleHTTP_FailoverOn503(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"loading model"}`)
		}))
		defer busy.Close()

		var gotBody string
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer good.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "busy", busy.URL, tc.advertisedModel))
		disc.AddManual(nodeForModel(t, "good", good.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("busy") // deterministic: busy is tried first

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (should have failed over past the 503)", rec.Code)
		}
		if gotBody != tc.inferenceBody() {
			t.Errorf("failover node got body %q, want the original request body", gotBody)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header on proxied success", got)
		}
	})
}

// TestHandleHTTP_AllNodesDownReturnsError: when every candidate fails at the
// transport, the client gets a 502 without added CORS permissions.
func TestHandleHTTP_AllNodesDownReturnsError(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		// Two servers we immediately close so dials fail.
		a := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		b := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		na := nodeForModel(t, "a", a.URL, tc.advertisedModel)
		nb := nodeForModel(t, "b", b.URL, tc.advertisedModel)
		a.Close()
		b.Close()

		disc := NewDiscovery()
		disc.AddManual(na)
		disc.AddManual(nb)
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 when all nodes are down", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header on exhausted error", got)
		}
	})
}

// TestHandleHTTP_404FailoverInferenceOnly: a 404 (model-not-found) on an
// inference call fails over to the next advertised owner, but a 404 on a
// non-inference path is returned as-is.
func TestHandleHTTP_404FailoverInferenceOnly(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"model not found"}`)
		}))
		defer missing.Close()
		has := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer has.Close()

		newProxy := func() *Proxy {
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "missing", missing.URL, tc.advertisedModel))
			disc.AddManual(nodeForModel(t, "has", has.URL, tc.advertisedModel))
			p := testProxy(tc.profile, disc, tc.profile.FacadePort)
			p.soleFacade().SetSelected("missing")
			return p
		}

		// Inference POST: 404 on first → fail over → 200.
		rec := httptest.NewRecorder()
		newProxy().soleFacade().handleHTTP(rec, tc.inferenceRequest())
		if rec.Code != http.StatusOK {
			t.Fatalf("inference 404: status = %d, want 200 (should fail over)", rec.Code)
		}

		// An ordinary non-inference GET still returns the first node's 404.
		rec = httptest.NewRecorder()
		newProxy().soleFacade().handleHTTP(rec, httptest.NewRequest(http.MethodGet, tc.nonInferencePath, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("non-inference 404: status = %d, want 404 (must NOT fail over)", rec.Code)
		}
	})
}

// TestHandleHTTP_AggregatesNativeModelList is Ollama-scoped because the
// native "models" dialect is Ollama's alone — LM Studio serves no /api/tags.
// The OpenAI dialect both engines share is covered by the sibling test below.
func TestHandleHTTP_AggregatesNativeModelList(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
				t.Errorf("upstream request = %s %s, want GET /api/tags", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Errorf("client credentials leaked to fan-out target")
			}
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(w, body)
		}))
	}
	a := server(`{"models":[{"name":"a","model":"a","digest":"a-only"},{"name":"shared","digest":"first"}]}`)
	defer a.Close()
	b := server(`{"models":[{"name":"shared:latest","model":"shared:latest","digest":"second"},{"name":"c","model":"c","digest":"c-only"}]}`)
	defer b.Close()
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"models":null}`)
	}))
	defer malformed.Close()
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downNode := nodeFor(t, "down", down.URL)
	down.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeFor(t, "a", a.URL))
	disc.AddManual(nodeFor(t, "b", b.URL))
	disc.AddManual(downNode)
	disc.AddManual(nodeFor(t, "malformed", malformed.URL))
	events := &recRW{}
	p := newTestProxy(ollamaOnlyProfile(t), NewCodec(events), disc, 11434)
	p.soleFacade().SetSelected("a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Cookie", "session=client-secret")
	done := make(chan struct{})
	go func() {
		p.soleFacade().handleHTTP(rec, req)
		close(done)
	}()

	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("model-list requests were not issued concurrently")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("aggregate request did not finish")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 3 || got.Models[0].Model != "a" || got.Models[1].Name != "shared" || got.Models[1].Model != "" || got.Models[2].Model != "c" {
		t.Fatalf("models = %+v, want a, shared, c", got.Models)
	}
	if got.Models[1].Digest != "first" {
		t.Errorf("duplicate metadata = %q, want deterministic first candidate", got.Models[1].Digest)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header", got)
	}
	// Addressed to the engine, like every facade-scoped notification: the
	// broker's process-scoped router claims only workload and node-activity
	// methods, so an unaddressed request event is dropped rather than relayed.
	if !events.has(`"method":"ollama:proxy/request-started"`) ||
		!events.has(`"method":"ollama:proxy/request"`) ||
		!events.has(`"target":"cluster"`) {
		t.Errorf("aggregate telemetry missing paired addressed cluster events: %s", events.b)
	}
}

// TestHandleHTTP_AggregatesOpenAIModelList runs for both engines: /v1/models
// is the one model-list route they share, and the dedupe must keep the first
// candidate's metadata on either.
func TestHandleHTTP_AggregatesOpenAIModelList(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		serve := func(body string) *httptest.Server {
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
					t.Errorf("upstream request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
				}
				_, _ = io.WriteString(w, body)
			}))
		}
		a := serve(`{"object":"list","data":[{"id":"a","owned_by":"a"},{"id":"shared","owned_by":"first"}]}`)
		defer a.Close()
		b := serve(`{"object":"list","data":[{"id":"shared","owned_by":"second"},{"id":"c","owned_by":"b"}]}`)
		defer b.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeFor(t, "a", a.URL))
		disc.AddManual(nodeFor(t, "b", b.URL))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("a")
		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

		var got struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || got.Object != "list" || len(got.Data) != 3 {
			t.Fatalf("response = %d %+v", rec.Code, got)
		}
		if got.Data[0].ID != "a" || got.Data[1].ID != "shared" || got.Data[1].OwnedBy != "first" || got.Data[2].ID != "c" {
			t.Fatalf("models = %+v, want a, shared(first), c", got.Data)
		}
	})
}

func TestHandleHTTP_ModelListEmptyAndUnavailable(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		serveEmpty := func() *httptest.Server {
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.emptyModelList)
			}))
		}

		// An unreachable sole node is 503, not an empty list: the client must
		// be able to tell "nothing is answering" from "nothing is loaded".
		down := serveEmpty()
		downNode := nodeFor(t, "empty", down.URL)
		down.Close()

		disc := NewDiscovery()
		disc.AddManual(downNode)
		rec := httptest.NewRecorder()
		testProxy(tc.profile, disc, tc.profile.FacadePort).soleFacade().
			handleHTTP(rec, httptest.NewRequest(http.MethodGet, tc.modelListPath, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unavailable status = %d, want 503", rec.Code)
		}

		// A reachable node with no models is 200 and this engine's own empty
		// envelope, so its client can parse the response.
		empty := serveEmpty()
		defer empty.Close()
		disc = NewDiscovery()
		disc.AddManual(nodeFor(t, "empty", empty.URL))
		rec = httptest.NewRecorder()
		testProxy(tc.profile, disc, tc.profile.FacadePort).soleFacade().
			handleHTTP(rec, httptest.NewRequest(http.MethodGet, tc.modelListPath, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != tc.emptyModelList {
			t.Fatalf("empty response = %d %s, want 200 %s", rec.Code, rec.Body.String(), tc.emptyModelList)
		}
	})
}

// TestHandleHTTP_StrictModelRouting proves capability is a gate before
// selection and priority, on both engines. The matching node advertises the
// model in the engine's own spelling — which for Ollama differs from what the
// client requests, so this also pins that normalizeModel is applied on the
// gate rather than only on the model list.
func TestHandleHTTP_StrictModelRouting(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		missHits, unknownHits, matchHits := 0, 0, 0
		miss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			missHits++
			w.WriteHeader(http.StatusOK)
		}))
		defer miss.Close()
		unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			unknownHits++
			w.WriteHeader(http.StatusOK)
		}))
		defer unknown.Close()
		match := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			matchHits++
			body, _ := io.ReadAll(r.Body)
			if string(body) != tc.inferenceBody() {
				t.Errorf("matching node got body %q", body)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer match.Close()

		disc := NewDiscovery()
		missNode := nodeFor(t, "selected-miss", miss.URL)
		missNode.Models = []string{"a-model-this-engine-does-not-have"}
		unknownNode := nodeFor(t, "a-unknown", unknown.URL)
		matchNode := nodeFor(t, "z-match", match.URL)
		matchNode.Models = []string{tc.advertisedModel}
		disc.AddManual(missNode)
		disc.AddManual(unknownNode)
		disc.AddManual(matchNode)
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("selected-miss")
		p.SetPriority([]string{"a-unknown", "selected-miss", "z-match"})
		candidates := p.soleFacade().resolveCandidates(tc.requestedModel)
		if len(candidates) != 1 || candidates[0].id != "z-match" {
			t.Fatalf("model candidates = %v, want only z-match", candidates)
		}

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if missHits != 0 || unknownHits != 0 || matchHits != 1 {
			t.Fatalf("hits miss=%d unknown=%d match=%d, want 0/0/1", missHits, unknownHits, matchHits)
		}

		// Capability filtering applies only to model-bearing inference.
		rec = httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, httptest.NewRequest(http.MethodGet, tc.nonInferencePath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("non-inference status = %d, want 200", rec.Code)
		}
		if missHits != 1 || unknownHits != 0 || matchHits != 1 {
			t.Fatalf("non-inference hits miss=%d unknown=%d match=%d, want 1/0/1", missHits, unknownHits, matchHits)
		}
	})
}

func TestHandleHTTP_NoAdvertisedModelRejectsLocally(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		hits := 0
		upstream := func() *httptest.Server {
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits++
				w.WriteHeader(http.StatusOK)
			}))
		}
		missing := upstream()
		defer missing.Close()
		unknown := upstream()
		defer unknown.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "missing", missing.URL, "a-model-this-engine-does-not-have"))
		disc.AddManual(nodeFor(t, "unknown", unknown.URL))
		events := &recRW{}
		p := newTestProxy(tc.profile, NewCodec(events), disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("missing")

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusBadGateway ||
			!strings.Contains(rec.Body.String(), "no available node advertises the requested model") {
			t.Fatalf("response = %d %s, want actionable local 502", rec.Code, rec.Body.String())
		}
		if hits != 0 {
			t.Fatalf("ineligible upstreams received %d requests, want 0", hits)
		}
		if !events.has("no node advertises requested model") {
			t.Fatalf("missing rejected request event: %s", events.b)
		}
	})
}

// TestResolveCandidates_SelfGuard: a node resolving to the proxy's own
// listen address is dropped so we never self-forward.
func TestResolveCandidates_SelfGuard(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		port := tc.profile.FacadePort
		disc := NewDiscovery()
		disc.AddManual(Node{ID: "self", Addresses: []string{"127.0.0.1"}, Port: port})
		disc.AddManual(Node{ID: "real", Addresses: []string{"192.0.2.10"}, Port: port})
		p := testProxy(tc.profile, disc, port)

		cands := p.soleFacade().resolveCandidates("")
		var haveReal bool
		for _, c := range cands {
			if c.id == "self" {
				t.Errorf("self-target node must be excluded, got candidate %+v", c)
			}
			if c.id == "real" {
				haveReal = true
			}
		}
		if !haveReal {
			t.Errorf("expected the real node to survive the self-guard, candidates = %+v", cands)
		}
	})
}
