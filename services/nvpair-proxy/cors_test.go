// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func corsModels(tc engineCase) string {
	if tc.profile.Name == "ollama" {
		return `{"models":[{"name":"private-model"}]}`
	}
	return `{"object":"list","data":[{"id":"private-model"}]}`
}

func corsRequest(method, path, origin string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "127.0.0.1:40000"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if method == "OPTIONS" {
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "Content-Type")
	}
	return r
}
func corsEngine(t *testing.T, tc engineCase, allowed string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed != "" && (r.Header.Get("Origin") == allowed || allowed == "*") {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
			_, err := io.WriteString(w, "engine refused")
			assert.NoError(t, err)
			return
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		_, err := io.WriteString(w, corsModels(tc))
		assert.NoError(t, err)
	}))
}
func proxyForCORSTargets(t *testing.T, tc engineCase, servers ...*httptest.Server) *facade {
	d := NewDiscovery()
	for i, s := range servers {
		d.AddManual(nodeFor(t, string(rune('a'+i)), s.URL))
	}
	return testProxy(tc.profile, d, tc.profile.StandalonePort).soleFacade()
}
func TestCORSOriginDenialIsNotRewritten(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		for _, status := range []int{200, 401, 403} {
			t.Run(http.StatusText(status), func(t *testing.T) {
				engine := corsEngine(t, tc, "", status)
				defer engine.Close()
				p := proxyForCORSTargets(t, tc, engine)
				for _, method := range []string{"OPTIONS", "GET"} {
					rec := httptest.NewRecorder()
					p.handlePlain(rec, corsRequest(method, "/policy", "http://example.com"))
					want := status
					if method == "OPTIONS" && status == 200 {
						want = 204
					}
					require.Equal(t, want, rec.Code, " (%v)", method)
					require.Equal(t, "", rec.Header().Get("Access-Control-Allow-Origin"), " (%v)", method)
					if status != 200 {
						require.Equal(t, "engine refused", rec.Body.String(), "upstream error body changed")
					}
				}
			})
		}
	})
}

// A preflight is shared only where every engine that answered agrees. The
// request it precedes would route around an engine that is down, so that
// engine's silence must not deny the browser the ones that are up.
func TestCORSClusterRequiresEveryRespondingTarget(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		for _, policy := range []struct {
			name, origin string
			status, want int
		}{
			{"agree", "http://app.test", 200, 204}, {"different origin", "http://other.test", 200, 403},
			{"missing policy", "", 200, 403}, {"denied", "", 403, 403}, {"unavailable", "", 503, 204},
		} {
			t.Run(policy.name, func(t *testing.T) {
				a := corsEngine(t, tc, "http://app.test", 200)
				defer a.Close()
				b := corsEngine(t, tc, policy.origin, policy.status)
				defer b.Close()
				p := proxyForCORSTargets(t, tc, a, b)
				rec := httptest.NewRecorder()
				p.handlePlain(rec, corsRequest("OPTIONS", tc.inferencePath, "http://app.test"))
				require.Equal(t, policy.want, rec.Code, "status")
				if policy.want == 204 {
					require.NotEmpty(t, rec.Header().Get("Access-Control-Max-Age"), "invalid agreement")
					require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"), "invalid agreement")
				} else {
					require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "granted denied origin")
				}
			})
		}
	})
}

// The model list is shared only where every engine that answered agrees. An
// engine that could not answer has no permission to intersect: its models are
// absent from the merged list either way, so treating its silence as a denial
// would let one offline node cut a browser off from every online one.
func TestCORSModelListRequiresEveryRespondingTarget(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		for _, policy := range []struct {
			name, origin string
			status, want int
		}{
			{"agree", "http://app.test", 200, 200}, {"different origin", "http://other.test", 200, 403},
			{"missing policy", "", 200, 403}, {"denied", "", 403, 403}, {"unavailable", "", 503, 200},
		} {
			t.Run(policy.name, func(t *testing.T) {
				a := corsEngine(t, tc, "http://app.test", 200)
				defer a.Close()
				b := corsEngine(t, tc, policy.origin, policy.status)
				defer b.Close()
				p := proxyForCORSTargets(t, tc, a, b)
				rec := httptest.NewRecorder()
				p.handlePlain(rec, corsRequest("GET", tc.modelListPath, "http://app.test"))
				require.Equal(t, policy.want, rec.Code, "status")
				if policy.want != 200 {
					require.NotContains(t, rec.Body.String(), "private-model", "partial inventory exposed")
					require.Equal(t, "", rec.Header().Get("Access-Control-Allow-Origin"), "partial inventory exposed")
				} else {
					require.Equal(t, "http://app.test", rec.Header().Get("Access-Control-Allow-Origin"), "shared permissions missing")
					require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"), "shared permissions missing")
				}
			})
		}
	})
}

// An engine that is down must not be able to deny the browser, but one that
// answers and refuses the origin still must.
func TestCORSModelListSeparatesSilenceFromDenial(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		allowing := corsEngine(t, tc, "http://app.test", 200)
		defer allowing.Close()
		offline := corsEngine(t, tc, "", 503)
		defer offline.Close()

		rec := httptest.NewRecorder()
		proxyForCORSTargets(t, tc, allowing, offline).
			handlePlain(rec, corsRequest("GET", tc.modelListPath, "http://app.test"))
		require.Equal(t, 200, rec.Code, "an offline engine denied the browser: status")
		require.Equal(t, "http://app.test", rec.Header().Get("Access-Control-Allow-Origin"), "an offline engine denied the browser: status")
		require.Contains(t, rec.Body.String(), "private-model", "the reachable engine's inventory was dropped")

		refusing := corsEngine(t, tc, "http://other.test", 200)
		defer refusing.Close()
		rec = httptest.NewRecorder()
		proxyForCORSTargets(t, tc, allowing, refusing).
			handlePlain(rec, corsRequest("GET", tc.modelListPath, "http://app.test"))
		require.Equal(t, 403, rec.Code, "a live refusal was overridden: status")
		require.NotContains(t, rec.Body.String(), "private-model", "a live refusal was overridden: status")
	})
}
func TestCORSNoRetryOnPermissionDenial(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		for _, status := range []int{401, 403} {
			a := corsEngine(t, tc, "", status)
			var calls atomic.Int32
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
			p := proxyForCORSTargets(t, tc, a, b)
			rec := httptest.NewRecorder()
			p.handlePlain(rec, corsRequest("GET", "/policy", "http://app.test"))
			require.Equal(t, status, rec.Code, "retried a permission denial")
			require.Equal(t, int32(0), calls.Load(), "retried a permission denial")
			a.Close()
			b.Close()
		}
	})
}
func TestCORSOrdinaryOptionsAndPolicyChanges(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		var origin atomic.Value
		origin.Store("http://app.test")
		var calls atomic.Int32
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", origin.Load().(string))
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(200)
		}))
		defer a.Close()
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(200)
		}))
		defer b.Close()
		p := proxyForCORSTargets(t, tc, a, b)
		req := httptest.NewRequest("OPTIONS", "/policy", nil)
		rec := httptest.NewRecorder()
		p.handleHTTP(rec, req)
		require.Equal(t, 200, rec.Code, "ordinary OPTIONS was synthesized or fanned out")
		require.Equal(t, int32(0), calls.Load(), "ordinary OPTIONS was synthesized or fanned out")
		rec = httptest.NewRecorder()
		p.handleHTTP(rec, corsRequest("OPTIONS", "/policy", "http://app.test"))
		require.Equal(t, 204, rec.Code, "initial agreement failed")
		origin.Store("http://other.test")
		rec = httptest.NewRecorder()
		p.handleHTTP(rec, corsRequest("OPTIONS", "/policy", "http://app.test"))
		require.Equal(t, 403, rec.Code, "cached obsolete permission")
	})
}
func TestCORSModelListStripsCredentialsAndDoesNotRedirect(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		var leaked atomic.Int32
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
		defer sink.Close()
		for _, redirect := range []bool{false, true} {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "http://app.test", r.Header.Get("Origin"), "CORS inputs lost")
				assert.Equal(t, "end-to-end", r.Header.Get("X-Test"), "CORS inputs lost")
				assert.Empty(t, r.Header.Get("Authorization"), "caller credentials forwarded to model-list candidate")
				assert.Empty(t, r.Header.Get("Cookie"), "caller credentials forwarded to model-list candidate")
				assert.Equal(t, "", r.Header.Get("X-Hop"), "hop header forwarded")
				w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
				w.Header().Set("Vary", "X-Test")
				if redirect {
					http.Redirect(w, r, sink.URL, 307)
					return
				}
				_, err := io.WriteString(w, corsModels(tc))
				assert.NoError(t, err)
			}))
			p := proxyForCORSTargets(t, tc, upstream, upstream)
			req := corsRequest("GET", tc.modelListPath, "http://app.test")
			req.Header.Set("Authorization", "Bearer test")
			req.Header.Set("Cookie", "session=test")
			req.Header.Set("X-Test", "end-to-end")
			req.Header.Set("Connection", "X-Hop")
			req.Header.Set("X-Hop", "local")
			rec := httptest.NewRecorder()
			p.handlePlain(rec, req)
			want := 200
			if redirect {
				want = 502
			}
			require.Equal(t, want, rec.Code, "status")
			if !redirect {
				require.Contains(t, strings.Join(rec.Header().Values("Vary"), ","), "X-Test", "Vary lost")
			}
			upstream.Close()
		}
		require.Equal(t, int32(0), leaked.Load(), "followed aggregate redirect")
	})
}
func TestCORSPairedIngressPreservesPolicyAndIsTerminal(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		aDir, bDir := t.TempDir(), t.TempDir()
		clustertrusttest.Join(t, aDir, "cluster", "a")
		clustertrusttest.Join(t, bDir, "cluster", "b")
		pin := func(dst, src, id string) {
			t.Helper()
			pem, err := os.ReadFile(filepath.Join(src, "node.crt"))
			require.NoError(t, err)
			body, err := json.Marshal(map[string]string{"nodeUuid": id, "certPem": string(pem)})
			assert.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(dst, "trusted"), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(dst, "trusted", id+".json"), body, 0600))
		}
		pin(aDir, bDir, "b")
		pin(bDir, aDir, "a")
		local := corsEngine(t, tc, "http://app.test", 200)
		defer local.Close()
		var unexpected atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { unexpected.Add(1) }))
		defer other.Close()
		peer := proxyForCORSTargets(t, tc, other)
		peer.host.mesh = clustertrust.Open(bDir)
		backend := nodeFor(t, "local", local.URL)
		require.NoError(t, peer.setLocalBackend(localBackend{Host: backend.Addresses[0], Port: backend.Port, Healthy: true}))
		ingress := httptest.NewUnstartedServer(http.HandlerFunc(peer.handleClusterIngress))
		ingress.TLS = peer.host.mesh.ServerTLSConfig()
		ingress.StartTLS()
		defer ingress.Close()
		caller := testProxy(tc.profile, NewDiscovery(), tc.profile.StandalonePort).soleFacade()
		caller.host.mesh = clustertrust.Open(aDir)
		n := nodeFor(t, "peer", ingress.URL)
		n.ClusterUUID = "b"
		caller.discovery.SetSubscribed([]Node{n})
		for _, origin := range []string{"http://app.test", "http://denied.test"} {
			for _, method := range []string{"OPTIONS", "GET"} {
				rec := httptest.NewRecorder()
				caller.handlePlain(rec, corsRequest(method, "/policy", origin))
				expected := ""
				if origin == "http://app.test" {
					expected = origin
				}
				require.Equal(t, expected, rec.Header().Get("Access-Control-Allow-Origin"), "paired policy lost")
				require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, "paired status")
			}
		}
		require.Equal(t, int32(0), unexpected.Load(), "paired ingress routed onward")
	})
}

// Opt-in fixture for real browser validation; regular suites do not keep a server running.
func TestCORSBrowserFixture(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		file := os.Getenv("PAIR_CORS_BROWSER_FIXTURE")
		if file == "" {
			t.Skip("browser fixture is opt-in")
		}
		page := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, err := io.WriteString(w, "<!doctype html><title>CORS test</title>")
			assert.NoError(t, err)
		}
		allowed := httptest.NewServer(http.HandlerFunc(page))
		defer allowed.Close()
		denied := httptest.NewServer(http.HandlerFunc(page))
		defer denied.Close()
		engine := corsEngine(t, tc, allowed.URL, 200)
		defer engine.Close()
		second := corsEngine(t, tc, allowed.URL, 200)
		defer second.Close()
		p := proxyForCORSTargets(t, tc, engine, second)
		proxy := httptest.NewServer(http.HandlerFunc(p.handlePlain))
		defer proxy.Close()
		none := proxyForCORSTargets(t, tc)
		unavailable := httptest.NewServer(http.HandlerFunc(none.handlePlain))
		defer unavailable.Close()
		data, err := json.Marshal(map[string]string{"allowed": allowed.URL, "denied": denied.URL, "proxy": proxy.URL, "engine": engine.URL, "unavailable": unavailable.URL, "models": tc.modelListPath})
		assert.NoError(t, err)
		require.NoError(t, os.WriteFile(file, data, 0600))
		deadline := time.After(60 * time.Second)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-deadline:
				require.FailNow(t, "browser fixture timed out")
			case <-tick.C:
				if _, err := os.Stat(file + ".done"); err == nil {
					require.NoError(t, os.Remove(file+".done"))
					return
				}
			}
		}
	})
}

// TestCORSExternalEngineParity optionally compares a running engine with this
// proxy implementation using read-only OPTIONS and GET requests.
func TestCORSExternalEngineParity(t *testing.T) {
	tc := ollamaCase(t)
	base := os.Getenv("PAIR_CORS_PARITY_URL")
	if executable := os.Getenv("PAIR_CORS_OLLAMA_EXECUTABLE"); base == "" && executable != "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		address := ln.Addr().String()
		require.NoError(t, ln.Close())
		base = "http://" + address
		ctx, cancel := context.WithCancel(context.Background())
		command := exec.CommandContext(ctx, executable, "serve")
		command.Env = append(os.Environ(), "OLLAMA_HOST="+address, "OLLAMA_ORIGINS=http://wrong.com", "OLLAMA_MODELS="+t.TempDir())
		if err := command.Start(); err != nil {
			cancel()
			require.NoError(t, err)
		}
		t.Cleanup(func() { cancel(); _ = command.Wait() })
		ready := false
		client := &http.Client{Timeout: time.Second}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			resp, err := client.Get(base + "/api/version")
			if err == nil {
				require.NoError(t, resp.Body.Close())
				if resp.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		require.True(t, ready, "isolated Ollama did not become ready")
	}
	if base == "" {
		t.Skip("external engine parity is opt-in")
	}
	d := NewDiscovery()
	d.AddManual(nodeFor(t, "engine", base))
	p := testProxy(tc.profile, d, tc.profile.StandalonePort).soleFacade()
	proxy := httptest.NewServer(http.HandlerFunc(p.handlePlain))
	defer proxy.Close()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, origin := range []string{"http://wrong.com", "http://example.com"} {
		for _, method := range []string{"OPTIONS", "GET"} {
			fetch := func(target string) (*http.Response, string) {
				req, err := http.NewRequest(method, target+"/", nil)
				require.NoError(t, err)
				req.Header.Set("Origin", origin)
				if method == "OPTIONS" {
					req.Header.Set("Access-Control-Request-Method", "GET")
				}
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer func() {
					assert.NoError(t, resp.Body.Close())
				}()
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				return resp, string(body)
			}
			direct, directBody := fetch(base)
			forwarded, body := fetch(proxy.URL)
			require.Equal(t, direct.StatusCode, forwarded.StatusCode, " (%v, %v)", method, origin)
			require.Equal(t, directBody, body, " (%v, %v)", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Allow-Origin"), ","), strings.Join(forwarded.Header.Values("Access-Control-Allow-Origin"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Allow-Headers"), ","), strings.Join(forwarded.Header.Values("Access-Control-Allow-Headers"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Allow-Methods"), ","), strings.Join(forwarded.Header.Values("Access-Control-Allow-Methods"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Allow-Credentials"), ","), strings.Join(forwarded.Header.Values("Access-Control-Allow-Credentials"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Max-Age"), ","), strings.Join(forwarded.Header.Values("Access-Control-Max-Age"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Access-Control-Expose-Headers"), ","), strings.Join(forwarded.Header.Values("Access-Control-Expose-Headers"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			require.Equal(t, strings.Join(direct.Header.Values("Vary"), ","), strings.Join(forwarded.Header.Values("Vary"), ","), "%s origin=%s: CORS header must match engine response", method, origin)
			t.Logf("%s origin=%s direct=%d proxy=%d allow-origin=%q", method, origin, direct.StatusCode, forwarded.StatusCode, forwarded.Header.Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestCORSInvalidModelListIsNotPartiallyExposed(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		good := corsEngine(t, tc, "http://app.test", 200)
		defer good.Close()
		for _, invalid := range []string{"not json", "{}", "null"} {
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
				_, err := io.WriteString(w, invalid)
				assert.NoError(t, err)
			}))
			p := proxyForCORSTargets(t, tc, good, bad)
			rec := httptest.NewRecorder()
			p.handlePlain(rec, corsRequest("GET", tc.modelListPath, "http://app.test"))
			require.Equal(t, http.StatusBadGateway, rec.Code, "invalid inventory exposed")
			require.Equal(t, "http://app.test", rec.Header().Get("Access-Control-Allow-Origin"), "invalid inventory exposed")
			require.NotContains(t, rec.Body.String(), "private-model", "invalid inventory exposed")
			bad.Close()
		}
	})
}
func TestCORSStreamingResponseIsPreserved(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "http://app.test", r.Header.Get("Origin"), "lost stream origin")
			w.Header().Set("Access-Control-Allow-Origin", "http://app.test")
			w.Header().Set("Access-Control-Expose-Headers", "X-Engine")
			w.Header().Set("X-Engine", "metadata")
			_, err := io.WriteString(w, "first")
			assert.NoError(t, err)
			w.(http.Flusher).Flush()
			select {
			case <-release:
				_, err := io.WriteString(w, "last")
				assert.NoError(t, err)
			case <-r.Context().Done():
			}
		}))
		defer upstream.Close()
		p := proxyForCORSTargets(t, tc, upstream)
		proxy := httptest.NewServer(http.HandlerFunc(p.handlePlain))
		defer proxy.Close()
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		req, err := http.NewRequest("GET", proxy.URL+"/stream", nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "http://app.test")
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() {
			assert.NoError(t, resp.Body.Close())
		}()
		first := make([]byte, 5)
		_, err = io.ReadFull(resp.Body, first)
		require.NoError(t, err)
		require.Equal(t, "first", string(first), "stream changed")
		require.Equal(t, "http://app.test", resp.Header.Get("Access-Control-Allow-Origin"), "stream changed")
		require.Equal(t, "X-Engine", resp.Header.Get("Access-Control-Expose-Headers"), "stream changed")
		close(release)
		rest, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "stream incomplete")
		require.Equal(t, "last", string(rest), "stream incomplete")
	})
}
