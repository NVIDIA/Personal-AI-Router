// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHandleHTTP_FailsOverOnlyForUnavailableModelPreparation(t *testing.T) {
	test := func(name, message string, status, wantStatus int, wantPeerCalls int32) {
		t.Run(name, func(t *testing.T) {
			const model = "org/model"
			var localCalls, peerCalls atomic.Int32
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				localCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(local.Close)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				peerCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"model":"org/model","choices":[]}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(peer.Close)
			facade, preparation := modelRequestFacade(t, local.URL, message)
			preparation.failureStatus = status
			facade.discovery.AddManual(nodeForModel(t, "local-owner", local.URL, model))
			facade.discovery.AddManual(nodeForModel(t, "peer-owner", peer.URL, model))
			flatPriority(facade.host, 1, "local-owner", "peer-owner")
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/model","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			facade.handleHTTP(response, request)

			assert.Len(t, preparation.requests, 1)
			assert.Zero(t, localCalls.Load(), "a rejected preparation must not forward inference to the local engine")
			assert.Equal(t, wantPeerCalls, peerCalls.Load())
			assert.Equal(t, wantStatus, response.Code)
		})
	}

	test("starting engine uses a healthy peer", `engine is starting: "vllm"`, http.StatusServiceUnavailable, http.StatusOK, 1)
	test("stale enabled inventory uses a healthy peer", `engine is disabled: "vllm"`, http.StatusServiceUnavailable, http.StatusOK, 1)
	test("a genuine conflict remains an error", "model preparation conflicts with the request", http.StatusConflict, http.StatusConflict, 0)
	test("a model loading failure remains an error", "model does not fit in memory", http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, 0)
}
