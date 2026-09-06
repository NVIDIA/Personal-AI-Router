// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
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

func testProxy(disc *Discovery, port int) *Proxy {
	return NewProxy(NewCodec(rwNop{}), disc, port)
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

// TestHandlePlain_OptionsPreflight: a CORS preflight is answered locally with
// 204 + permissive headers and never forwarded.
func TestHandlePlain_OptionsPreflight(t *testing.T) {
	p := testProxy(NewDiscovery(), 11434)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Token")
	p.handlePlain(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("missing Access-Control-Allow-Methods")
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "*" {
		t.Errorf("Access-Control-Expose-Headers = %q, want *", got)
	}
	// The browser's requested headers are echoed so an arbitrary header clears preflight.
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom-Token" {
		t.Errorf("Access-Control-Allow-Headers = %q, want echoed X-Custom-Token", got)
	}
}

// TestHandlePlain_EngineCredentialedPreflightPreserved: when an engine opts an
// exact origin into credentialed CORS, its preflight policy reaches the browser
// instead of being replaced by the proxy's uncredentialed wildcard fallback.
func TestHandlePlain_EngineCredentialedPreflightPreserved(t *testing.T) {
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
	p := testProxy(disc, 11434)
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	rec := httptest.NewRecorder()

	p.handlePlain(rec, req)

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
}

// TestHandleHTTP_EngineCORSPolicyPreserved: an engine that declares its own
// origin policy keeps it. Replacing it with the proxy's wildcard would widen
// what the user configured, and would break a credentialed response outright.
func TestHandleHTTP_EngineCORSPolicyPreserved(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://app.example")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the engine's own origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want the engine's true", got)
	}
}

// TestHandleHTTP_EngineCredentialsWithoutOriginDropped: an engine (or an
// intermediary in front of it) that sends Allow-Credentials but no origin has
// declared no policy to keep, so the proxy supplies its own. The wildcard it
// writes is invalid next to Allow-Credentials: true, and a browser rejects that
// pair, so the inherited header must not survive the forward.
func TestHandleHTTP_EngineCredentialsWithoutOriginDropped(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer engine.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the proxy's wildcard", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want cleared alongside the wildcard origin", got)
	}
}

// TestHandleHTTP_HappyPathSingleNode: the common case — one healthy node
// answers directly, body forwarded, CORS present on the success response.
func TestHandleHTTP_HappyPathSingleNode(t *testing.T) {
	var gotBody string
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer good.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "good", good.URL, "llama"))
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotBody != `{"model":"llama"}` {
		t.Errorf("node got body %q, want the original request body", gotBody)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * on success", got)
	}
}

// TestHandleHTTP_NoRetryOn400: a client error (400) is returned as-is and not
// failed over — retrying elsewhere would return the same error and mask it.
func TestHandleHTTP_NoRetryOn400(t *testing.T) {
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
	disc.AddManual(nodeForModel(t, "bad", bad.URL, "llama"))
	disc.AddManual(nodeForModel(t, "other", other.URL, "llama"))
	p := testProxy(disc, 11434)
	p.SetSelected("bad")

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (client errors must not fail over)", rec.Code)
	}
	if hits != 1 {
		t.Errorf("bad node hit %d times, want exactly 1 (no retry on 400)", hits)
	}
}

// TestHandleHTTP_RejectionHasCORS: even the no-node rejection carries CORS so a
// browser sees the real 502 instead of an opaque CORS error.
func TestHandleHTTP_RejectionHasCORS(t *testing.T) {
	p := testProxy(NewDiscovery(), 11434)
	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * on rejection", got)
	}
}

// TestHandleHTTP_FailoverOn503: a busy first node (503) is skipped and the
// request is filled by the next node, with the original body replayed.
func TestHandleHTTP_FailoverOn503(t *testing.T) {
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
	disc.AddManual(nodeForModel(t, "busy", busy.URL, "llama"))
	disc.AddManual(nodeForModel(t, "good", good.URL, "llama"))
	p := testProxy(disc, 11434)
	p.SetSelected("busy") // deterministic: busy is tried first

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should have failed over past the 503)", rec.Code)
	}
	if gotBody != `{"model":"llama"}` {
		t.Errorf("failover node got body %q, want the original request body", gotBody)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * on proxied success", got)
	}
}

// TestHandleHTTP_AllNodesDownReturnsError: when every candidate fails at the
// transport, the client gets one clean 502 (not a hang), still with CORS.
func TestHandleHTTP_AllNodesDownReturnsError(t *testing.T) {
	// Two servers we immediately close so dials fail.
	a := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	na := nodeForModel(t, "a", a.URL, "llama")
	nb := nodeForModel(t, "b", b.URL, "llama")
	a.Close()
	b.Close()

	disc := NewDiscovery()
	disc.AddManual(na)
	disc.AddManual(nb)
	p := testProxy(disc, 11434)

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when all nodes are down", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * on exhausted error", got)
	}
}

// TestHandleHTTP_404FailoverInferenceOnly: a 404 (model-not-found) on an
// inference call fails over to the next advertised owner, but a 404 on a
// non-inference path is returned as-is.
func TestHandleHTTP_404FailoverInferenceOnly(t *testing.T) {
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
		disc.AddManual(nodeForModel(t, "missing", missing.URL, "llama"))
		disc.AddManual(nodeForModel(t, "has", has.URL, "llama"))
		p := testProxy(disc, 11434)
		p.SetSelected("missing")
		return p
	}

	// Inference POST: 404 on first → fail over → 200.
	rec := httptest.NewRecorder()
	newProxy().handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("inference 404: status = %d, want 200 (should fail over)", rec.Code)
	}

	// An ordinary non-inference GET still returns the first node's 404.
	rec = httptest.NewRecorder()
	newProxy().handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-inference 404: status = %d, want 404 (must NOT fail over)", rec.Code)
	}
}

func TestHandleHTTP_AggregatesModelList(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
				t.Errorf("upstream request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Errorf("client credentials leaked to fan-out target")
			}
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(w, body)
		}))
	}
	a := server(`{"object":"list","data":[{"id":"a","owned_by":"a-only"},{"id":"shared","owned_by":"first"}]}`)
	defer a.Close()
	b := server(`{"object":"list","data":[{"id":"shared","owned_by":"second"},{"id":"c","owned_by":"c-only"}]}`)
	defer b.Close()
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":null}`)
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
	p := testProxy(disc, 1234)
	p.SetSelected("a")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Cookie", "session=client-secret")
	done := make(chan struct{})
	go func() {
		p.handleHTTP(rec, req)
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
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != 3 || got.Data[0].ID != "a" || got.Data[1].ID != "shared" || got.Data[2].ID != "c" {
		t.Fatalf("models = %+v, want a, shared, c", got.Data)
	}
	if got.Data[1].OwnedBy != "first" {
		t.Errorf("duplicate metadata = %q, want deterministic first candidate", got.Data[1].OwnedBy)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestHandleHTTP_ModelListEmptyAndUnavailable(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	emptyNode := nodeFor(t, "empty", empty.URL)
	empty.Close()

	disc := NewDiscovery()
	disc.AddManual(emptyNode)
	rec := httptest.NewRecorder()
	testProxy(disc, 1234).handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d, want 503", rec.Code)
	}

	empty = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer empty.Close()
	disc = NewDiscovery()
	disc.AddManual(nodeFor(t, "empty", empty.URL))
	rec = httptest.NewRecorder()
	testProxy(disc, 1234).handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"object":"list","data":[]}` {
		t.Fatalf("empty response = %d %s, want 200 native empty list", rec.Code, rec.Body.String())
	}
}

// TestHandleHTTP_StrictModelRouting proves capability is a gate before
// selection and priority. Unknown and known-missing nodes are excluded from
// inference but remain available for non-inference routes.
func TestHandleHTTP_StrictModelRouting(t *testing.T) {
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
		if string(body) != `{"model":"llama"}` {
			t.Errorf("matching node got body %q", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer match.Close()

	disc := NewDiscovery()
	missNode := nodeFor(t, "selected-miss", miss.URL)
	missNode.Models = []string{"mistral"}
	unknownNode := nodeFor(t, "a-unknown", unknown.URL)
	matchNode := nodeFor(t, "z-match", match.URL)
	matchNode.Models = []string{"llama"}
	disc.AddManual(missNode)
	disc.AddManual(unknownNode)
	disc.AddManual(matchNode)
	p := testProxy(disc, 11434)
	p.SetSelected("selected-miss")
	p.SetPriority([]string{"a-unknown", "selected-miss", "z-match"})
	candidates := p.resolveCandidates("llama")
	if len(candidates) != 1 || candidates[0].id != "z-match" {
		t.Fatalf("model candidates = %v, want only z-match", candidates)
	}

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if missHits != 0 || unknownHits != 0 || matchHits != 1 {
		t.Fatalf("hits miss=%d unknown=%d match=%d, want 0/0/1", missHits, unknownHits, matchHits)
	}

	// Capability filtering applies only to model-bearing inference.
	rec = httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/unknown", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("non-inference status = %d, want 200", rec.Code)
	}
	if missHits != 1 || unknownHits != 0 || matchHits != 1 {
		t.Fatalf("non-inference hits miss=%d unknown=%d match=%d, want 1/0/1", missHits, unknownHits, matchHits)
	}
}

func TestHandleHTTP_NoAdvertisedModelRejectsLocally(t *testing.T) {
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
	disc.AddManual(nodeForModel(t, "missing", missing.URL, "mistral"))
	disc.AddManual(nodeFor(t, "unknown", unknown.URL))
	events := &prRec{}
	p := NewProxy(NewCodec(events), disc, 1234)
	p.SetSelected("missing")

	rec := httptest.NewRecorder()
	p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))

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
}

// TestResolveCandidates_SelfGuard: a node resolving to the proxy's own
// listen address is dropped so we never self-forward.
func TestResolveCandidates_SelfGuard(t *testing.T) {
	disc := NewDiscovery()
	disc.AddManual(Node{ID: "self", Addresses: []string{"127.0.0.1"}, Port: 11434})
	disc.AddManual(Node{ID: "real", Addresses: []string{"192.0.2.10"}, Port: 11434})
	p := testProxy(disc, 11434)

	cands := p.resolveCandidates("")
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
}
