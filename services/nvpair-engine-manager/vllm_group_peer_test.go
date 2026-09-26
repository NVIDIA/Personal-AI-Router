// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/reach"
)

func vllmGroupPeerFixture(t *testing.T, coordinatorLocal bool) (*vllmGroupPeer, vllmGroupPeerRequest, *x509.Certificate, string) {
	t.Helper()
	dir, mesh, certificate := remoteActionPin(t)
	local, other, rank := "host-b", "host-a", 1
	if coordinatorLocal {
		local, other, rank = "host-a", "host-b", 0
	}
	m := &Manager{mesh: mesh, peers: newPeerDirectory(), addrs: reach.NewChooser(), cableLocal: &cableLocalFacts{nodeID: local},
		remoteHTTP: clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{ResponseHeaderTimeout: remoteResponseHeaderTimeout}),
		readyHTTP:  clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{ResponseHeaderTimeout: remoteReadyResponseHeaderTimeout})}
	t.Cleanup(func() { m.remoteHTTP.CloseIdle(); m.readyHTTP.CloseIdle() })
	m.peers.peers[other] = ecPeer{nodeID: other, clusterUUID: "fixture-peer", addresses: []string{"192.0.2.2"}, port: 14323}
	model := "example/model@" + strings.Repeat("a", 40)
	plan := vllmGroupPlan{Coordinator: "host-a", Model: model, Runtime: vllmManagedVersion, Limits: vllmGroupLimitsForModel(model)}
	plan.Topology = vllmGroupTopology{TensorParallel: 2, PipelineParallel: 1, DataParallel: 1, ConfigSHA256: strings.Repeat("d", 64)}
	for i, node := range []string{"host-a", "host-b"} {
		principal := "fixture-peer"
		if node == local {
			principal = "fixture-self"
		}
		pin, ok := mesh.PinSHA256(principal)
		if !ok {
			t.Fatal("fixture pin missing")
		}
		gpu := "GPU-00000001-0000-0000-0000-000000000001"
		if i == 1 {
			gpu = "GPU-00000002-0000-0000-0000-000000000001"
		}
		plan.Members = append(plan.Members, vllmGroupMember{NodeID: node, PinSHA256: pin, GPUUUID: gpu, ModelDigest: strings.Repeat("b", 64), RuntimeDigest: strings.Repeat("c", 64), RuntimeCompatibilitySHA256: strings.Repeat("e", 64)})
	}
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	return &vllmGroupPeer{m: m}, vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, Action: "prepare", RunID: strings.Repeat("d", 32), Generation: 2, PlanDigest: digest, Plan: plan, Rank: rank}, certificate, dir
}

func TestVLLMGroupLegacyGenerationIsCleanupOnly(t *testing.T) {
	value := historicalRunObject(t)
	value["state"], value["cleanupConfirmed"] = "cleanup-required", false
	value["failure"] = "retained historical cleanup"
	for _, raw := range value["ranks"].([]any) {
		raw.(map[string]any)["cleanupConfirmed"] = false
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := parseRetainedVLLMGroupRun(data)
	if !ok || !run.legacyDigest {
		t.Fatal("historical cleanup fixture unavailable")
	}
	request := vllmGroupRequest(vllmGroupBinding{
		RunID: run.RunID, Generation: run.Generation, PlanDigest: run.PlanDigest,
		Plan: run.Plan, Rank: 0,
	}, "reconcile")
	for _, action := range []string{"prepare", "start", "ready"} {
		request.Action = action
		if err := validateVLLMGroupPeerRequest(request); err == nil {
			t.Fatalf("legacy %s admitted: %v", action, err)
		}
	}
	for _, action := range []string{"status", "stop", "reconcile"} {
		request.Action = action
		if err := validateVLLMGroupPeerRequest(request); err != nil {
			t.Fatalf("legacy cleanup action %s rejected: %v", action, err)
		}
	}
}

func TestVLLMGroupRetainedRuntimeCurrentReviewCanStart(t *testing.T) {
	_, request, _, _ := vllmGroupPeerFixture(t, true)
	request.Plan.Runtime = "0.28.0"
	request.PlanDigest, _ = vllmGroupPlanDigest(request.Plan)
	for _, action := range []string{"prepare", "start", "ready"} {
		request.Action = action
		if err := validateVLLMGroupPeerRequest(request); err != nil {
			t.Fatalf("current retained-runtime %s rejected: %v", action, err)
		}
	}
}

func TestVLLMGroupPeerPinnedControllerGetsOnlyBoundNativeRefusal(t *testing.T) {
	p, request, certificate, _ := vllmGroupPeerFixture(t, false)
	for _, action := range []string{"capability", "status", "prepare", "start", "ready", "stop", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			request.Action = action
			raw, _ := json.Marshal(request)
			r := httptest.NewRequest(http.MethodPost, vllmGroupPeerPath, bytes.NewReader(raw))
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
			w := httptest.NewRecorder()
			p.serveHTTP(w, r)
			var result vllmGroupPeerResult
			if w.Code != http.StatusOK || strictDiagnosticJSON(w.Body.Bytes(), &result) != nil || result != vllmGroupPeerRefusal(request) {
				t.Fatalf("incorrect bound refusal: HTTP %d %s", w.Code, w.Body.String())
			}
			if result.CleanupConfirmed || result.ActivationEnabled || result.EffectsApplied {
				t.Fatal("protocol response claimed native execution or clean ranks")
			}
		})
	}
}

func TestVLLMGroupPeerRefusesForeignBindingAndUnpinnedCaller(t *testing.T) {
	p, request, certificate, dir := vllmGroupPeerFixture(t, false)
	for _, change := range []string{"plaintext", "wrong-controller", "rank", "generation", "run", "digest", "pin", "action", "unknown-field", "trailing", "oversize"} {
		t.Run(change, func(t *testing.T) {
			r := request
			r.Plan = cloneVLLMGroupPlan(request.Plan)
			switch change {
			case "wrong-controller":
				if _, err := p.local(context.Background(), "fixture-self", r); err == nil {
					t.Fatal("non-coordinator principal admitted")
				}
				return
			case "rank":
				r.Rank = 0
			case "generation":
				r.Generation = 0
			case "run":
				r.RunID = "old-unbound-run"
			case "digest":
				r.PlanDigest = strings.Repeat("e", 64)
			case "pin":
				r.Plan.Members[0].PinSHA256 = strings.Repeat("e", 64)
				r.PlanDigest, _ = vllmGroupPlanDigest(r.Plan)
			case "action":
				r.Action = "exec"
			}
			raw, _ := json.Marshal(r)
			switch change {
			case "unknown-field":
				raw = append(raw[:len(raw)-1], []byte(`,"argv":["anything"]}`)...)
			case "trailing":
				raw = append(raw, []byte(` {}`)...)
			case "oversize":
				raw = append(raw, bytes.Repeat([]byte(" "), vllmGroupPeerMaxBytes)...)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, vllmGroupPeerPath, bytes.NewReader(raw))
			if change != "plaintext" {
				httpRequest.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
			}
			w := httptest.NewRecorder()
			p.serveHTTP(w, httpRequest)
			if w.Code < 400 {
				t.Fatalf("invalid %s admitted", change)
			}
		})
	}
	if err := os.Remove(filepath.Join(dir, "trusted", "fixture-peer.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.local(context.Background(), "fixture-peer", request); err == nil {
		t.Fatal("removed controller pin remained usable")
	}
}

func TestVLLMGroupPeerLocalControlCannotTurnRefusalIntoSuccess(t *testing.T) {
	p, request, _, _ := vllmGroupPeerFixture(t, true)
	for _, action := range []string{"capability", "status", "prepare", "start", "ready", "stop", "reconcile"} {
		request.Action = action
		result, err := p.control(context.Background(), request)
		if result != vllmGroupPeerRefusal(request) {
			t.Fatalf("binding lost for %s: %+v %v", action, result, err)
		}
		if action == "capability" || action == "status" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, errVLLMGroupNativeUnavailable) {
			t.Fatalf("%s did not retain native hold: %v", action, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.control(ctx, request); err == nil {
		t.Fatal("canceled control accepted")
	}
}

func TestVLLMGroupPeerRemoteReplyIsExactAndNoRedirectOrRetry(t *testing.T) {
	_, request, _, _ := vllmGroupPeerFixture(t, true)
	request.Rank = 1
	for _, change := range []string{"none", "run", "generation", "rank", "node", "controller", "digest", "activation", "cleanup", "effects", "redirect", "unknown", "oversize"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			client := &remoteClient{base: "https://192.0.2.2:14323", http: &http.Client{Transport: remoteActionTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != vllmGroupPeerPath || r.URL.Host != "192.0.2.2:14323" {
					t.Fatal("request selected a foreign endpoint")
				}
				raw, _ := io.ReadAll(r.Body)
				var sent vllmGroupPeerRequest
				if strictDiagnosticJSON(raw, &sent) != nil || sent.RunID != request.RunID || sent.Generation != request.Generation || sent.Rank != request.Rank || sent.PlanDigest != request.PlanDigest {
					t.Fatal("request ownership binding changed")
				}
				result := vllmGroupPeerRefusal(request)
				switch change {
				case "run":
					result.RunID = strings.Repeat("f", 32)
				case "generation":
					result.Generation++
				case "rank":
					result.Rank = 0
				case "node":
					result.NodeID = "foreign"
				case "controller":
					result.Controller = "foreign"
				case "digest":
					result.PlanDigest = strings.Repeat("f", 64)
				case "activation":
					result.ActivationEnabled = true
				case "cleanup":
					result.CleanupConfirmed = true
				case "effects":
					result.EffectsApplied = true
				}
				body, _ := json.Marshal(result)
				if change == "unknown" {
					body = append(body[:len(body)-1], []byte(`,"extra":true}`)...)
				}
				if change == "oversize" {
					body = append(body, bytes.Repeat([]byte(" "), vllmGroupPeerMaxBytes)...)
				}
				response := remoteActionResponse(r, http.StatusOK, string(body))
				if change == "redirect" {
					response.StatusCode = http.StatusTemporaryRedirect
					response.Header.Set("Location", "https://example.invalid/")
				}
				return response, nil
			})}}
			_, err := client.vllmGroupParticipant(context.Background(), request)
			if (err == nil) != (change == "none") || calls != 1 {
				t.Fatalf("reply %s: err=%v calls=%d", change, err, calls)
			}
		})
	}
}
