// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedRemoteUpdateUsesProductRouteAndRetainedFence(t *testing.T) {
	f := vllmResourceFixture(t)
	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{RunID: strings.Repeat("e", 32), Generation: 13, PlanDigest: digest, Plan: plan, State: "cleanup-required", Failure: "cleanup remains required"}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true})
	}
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, vllmGroupJournalFile), run); err != nil {
		t.Fatal(err)
	}

	_, mesh, certificate := remoteActionPin(t)
	server := (&controlServer{exec: f.e, mesh: mesh}).mux()
	body, _ := json.Marshal(updateRequest{OpID: "update-op", Engine: "vllm"})
	request := httptest.NewRequest(http.MethodPost, controlUpdatePath, bytes.NewReader(body))
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	frames := decodeFrames(t, recorder.Body.String())
	if recorder.Code != http.StatusOK || len(frames) != 1 || frames[0].Type != "error" || !strings.Contains(frames[0].Message, vllmGroupHeldReason) {
		t.Fatalf("held remote update response=%d frames=%+v", recorder.Code, frames)
	}

	unpinned := httptest.NewRecorder()
	server.ServeHTTP(unpinned, httptest.NewRequest(http.MethodPost, controlUpdatePath, bytes.NewReader(body)))
	if unpinned.Code != http.StatusForbidden {
		t.Fatalf("unpinned remote update status=%d", unpinned.Code)
	}
}

func TestRemoteUpdateClientStreamsExactEndpoint(t *testing.T) {
	calls := 0
	client := &remoteClient{base: "https://192.0.2.2:14323", http: &http.Client{Transport: remoteActionTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != controlUpdatePath || request.URL.Host != "192.0.2.2:14323" {
			t.Fatalf("remote update escaped pinned endpoint: %s %s", request.Method, request.URL.String())
		}
		return remoteActionResponse(request, http.StatusOK, `{"type":"result","opId":"op","engine":"vllm","op":"update","status":{"engine":"vllm","installed":true}}`+"\n"), nil
	})}}
	terminal, err := client.stream(context.Background(), controlUpdatePath, updateRequest{OpID: "op", Engine: "vllm"}, nil)
	if err != nil || calls != 1 || terminal.Status == nil || terminal.Status.Engine != "vllm" {
		t.Fatalf("remote update terminal=%+v calls=%d err=%v", terminal, calls, err)
	}
}

func TestRemoteUpdateClientRefusesRedirect(t *testing.T) {
	calls := 0
	client := &remoteClient{base: "https://192.0.2.2:14323", http: &http.Client{Transport: remoteActionTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls != 1 {
			t.Fatalf("remote update followed redirect to %s", request.URL)
		}
		response := remoteActionResponse(request, http.StatusTemporaryRedirect, "redirect refused")
		response.Header.Set("Location", "http://192.0.2.3:14323/escaped")
		return response, nil
	})}}
	_, err := client.stream(context.Background(), controlUpdatePath, updateRequest{OpID: "op", Engine: "vllm"}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || calls != 1 {
		t.Fatalf("redirect result calls=%d err=%v", calls, err)
	}
}
