// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVLLMCurrentModelsAfterSystemRankCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rank    int
		cleaned bool
	}{
		{"closed coordinator", 0, true},
		{"closed participant", 1, true},
		{"active participant", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vllmResourceFixture(t)
			proc := &managedProc{done: make(chan struct{})}
			f.st.proc, f.st.running, f.st.healthy = proc, true, true
			f.st.servingModel, f.st.version = "fixture-standalone", "0.28.0"
			state := "started"
			if tc.cleaned {
				state = "stopped"
			}
			rank := &vllmManagedRank{binding: vllmGroupBinding{Rank: tc.rank}, receipt: vllmRankReceipt{
				State: state, CleanupConfirmed: tc.cleaned, SystemPlan: &vllmRankSystemPlan{Model: "fixture-old-group"},
			}}
			f.st.vllmRank = rank
			retained := rank.receipt
			f.e.vllmOwnsListener = func(p *managedProc, port int) bool { return p == proc && port == f.st.port }
			calls := 0
			f.e.client = &http.Client{Transport: managedReadinessTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				body := "{}"
				switch req.URL.Path {
				case "/version":
					body = `{"version":"0.28.0"}`
				case "/v1/models":
					body = `{"object":"list","data":[{"id":"fixture-standalone","owned_by":"vllm"}]}`
				case "/health":
				default:
					t.Fatalf("unexpected inventory request: %s", req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			got, err := f.e.vllmCurrentModels(context.Background(), f.st)
			want, wantCalls := `{"data":[],"object":"list"}`, 0
			if tc.cleaned {
				want, wantCalls = `{"data":[{"id":"fixture-standalone","owned_by":"vllm"}],"object":"list"}`, 3
			}
			if err != nil || string(got) != want || calls != wantCalls {
				t.Fatalf("inventory=%s calls=%d; want=%s calls=%d; error=%v", got, calls, want, wantCalls, err)
			}
			if rank.receipt != retained || f.st.vllmRank != rank {
				t.Fatal("inventory changed the retained group receipt")
			}
		})
	}
}
