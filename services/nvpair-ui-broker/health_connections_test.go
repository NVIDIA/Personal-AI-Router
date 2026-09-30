// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/httpcon/testclient"
)

func TestHealthChecksReuseConnections(t *testing.T) {
	test := func(name, path string, profile engineProxyProfile) {
		t.Run(name, func(t *testing.T) {
			testFraming := func(name string, chunked bool) {
				t.Run(name, func(t *testing.T) {
					var requests atomic.Int32
					client, connections := testclient.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						assert.Equal(t, path, r.URL.Path, "health probe path")
						status := http.StatusOK
						if requests.Add(1)%2 == 0 {
							status = http.StatusServiceUnavailable
						}
						w.WriteHeader(status)
						if chunked {
							assert.NoError(t, http.NewResponseController(w).Flush())
						}
						_, writeErr := io.WriteString(w, `{"models":[],"status":"responding"}`)
						assert.NoError(t, writeErr)
					}))
					const rounds = 32
					for i := 0; i < rounds; i++ {
						require.Equal(t, i%2 == 0, checkEngineHealth(profile, client, 1), "poll %d", i)
					}
					require.Equal(t, int32(1), connections.Count(), "HTTP/1 connections for %d polls", rounds)
				})
			}

			testFraming("content-length", false)
			testFraming("chunked", true)
		})
	}

	test("ollama", "/", ollamaProxyProfile)
	test("lmstudio", "/v1/models", lmstudioProxyProfile)
	test("llamacpp", "/health", mustEngineProxyProfile("llamacpp"))
}
