// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"nvpair-shared/httpcon/testclient"
)

func TestHealthChecksReuseConnections(t *testing.T) {
	test := func(name, path string, profile engineProxyProfile) {
		t.Run(name, func(t *testing.T) {
			testFraming := func(name string, chunked bool) {
				t.Run(name, func(t *testing.T) {
					var requests atomic.Int32
					client, connections := testclient.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != path {
							t.Errorf("path = %q, want %q", r.URL.Path, path)
						}
						status := http.StatusOK
						if requests.Add(1)%2 == 0 {
							status = http.StatusServiceUnavailable
						}
						w.WriteHeader(status)
						if chunked {
							_ = http.NewResponseController(w).Flush()
						}
						_, _ = io.WriteString(w, `{"models":[],"status":"responding"}`)
					}))
					const rounds = 32
					for i := 0; i < rounds; i++ {
						if got, want := checkEngineHealth(profile, client, 1), i%2 == 0; got != want {
							t.Fatalf("poll %d: healthy = %v, want %v", i, got, want)
						}
					}
					if got := connections.Count(); got != 1 {
						t.Fatalf("accepted %d HTTP/1 connections for %d polls, want 1", got, rounds)
					}
				})
			}

			testFraming("content-length", false)
			testFraming("chunked", true)
		})
	}

	test("ollama", "/", ollamaProxyProfile)
	test("lmstudio", "/v1/models", lmstudioProxyProfile)
}
