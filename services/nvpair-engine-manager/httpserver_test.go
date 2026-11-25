// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// Inventory must report a timeout as unavailable. A successful empty list
// would make clients discard models when only the status probe was slow.
func TestModelsHandler_RejectsSweepThatExpiresDuringStatusProbe(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	ex := fixture.executor
	enableModelInventory(t, ex)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, modelsPath, nil).WithContext(ctx)
	response := httptest.NewRecorder()
	// Signal that the handler returned so its deadline can be checked separately.
	finished := make(chan struct{})
	go func() {
		modelsHandler(ex)(response, req)
		close(finished)
	}()
	awaitProbeStart(t, fixture.probeStarted)
	select {
	case <-finished:
	case <-time.After(time.Second):
		fixture.releaseProbe()
		require.FailNow(t, "inventory response ignored its deadline")
	}
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	fixture.releaseProbe()
}

// Model requests must stop waiting for an engine operation at the caller's
// deadline or the service's own limit, whichever expires first.
func TestModelsHandler_EnforcesDeadlinesDuringEngineOperation(t *testing.T) {
	test := func(name string, callerTimeout, wantElapsed time.Duration) {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt()))
				platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
				platform.Runtime.Ready = nil
				platform.Runtime.Health = nil
				m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
				ex := newTestExecutor(t, m)
				enableModelInventory(t, ex)
				st, err := ex.state("fake")
				require.NoError(t, err)
				st.lockOperation()
				defer st.unlockOperation()
				ctx := context.Background()
				if callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, callerTimeout)
					defer cancel()
				}
				req := httptest.NewRequest(http.MethodGet, modelsPath, nil).WithContext(ctx)
				response := httptest.NewRecorder()
				// Observe completion while the operation stays held. The outer
				// limit catches a handler that ignores its deadline.
				finished := make(chan struct{})
				started := time.Now()
				go func() {
					modelsHandler(ex)(response, req)
					close(finished)
				}()
				select {
				case <-finished:
				case <-time.After(wantElapsed + 2*time.Second):
					t.Fatal("model request kept waiting past its deadline")
				}
				require.Equal(t, wantElapsed, time.Since(started))
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
			})
		})
	}
	test("honors shorter caller deadline", 50*time.Millisecond, 50*time.Millisecond)
	test("enforces service deadline without caller deadline", 0, 5*time.Second)
}

// Keep the operation active until synctest confirms the model request is
// waiting. An in-memory engine response avoids network scheduling in the test.
func TestModelsHandler_ReturnsModelsAfterEngineOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt()))
		platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
		platform.Runtime.Ready = nil
		platform.Runtime.Health = nil
		m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
		ex := newTestExecutor(t, m)
		enableModelInventory(t, ex)
		ex.client = &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || req.URL.Path != "/api/tags" {
				return nil, fmt.Errorf("unexpected model request: %s %s", req.Method, req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"models":[{"name":"fixture-model"}]}`)),
			}, nil
		})}
		st, err := ex.state("fake")
		require.NoError(t, err)
		st.mu.Lock()
		st.running = true
		st.healthy = true
		st.adopted = true
		st.mu.Unlock()
		st.lockOperation()
		var once sync.Once
		unlock := func() { once.Do(st.unlockOperation) }
		defer unlock()
		req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
		response := httptest.NewRecorder()
		// Distinguish a request waiting for the operation from an early reply.
		finished := make(chan struct{})
		go func() {
			modelsHandler(ex)(response, req)
			close(finished)
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Fatal("model request returned before the operation finished")
		default:
		}
		unlock()
		awaitResult(t, finished)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		var result ModelsResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, []string{"fixture-model"}, result.Models)
		require.Equal(t, map[string][]string{"fake": {"fixture-model"}}, result.ByEngine)
	})
}

// The modelAliases key is additive on the /v1/models contract: peers running
// older scanners decode only the model lists, so nothing may change about the
// body until this node actually has an alias document. Once the broker delivers
// one (from the settings service, through the courier), the list rides every
// fetch and replaces whole, exactly as it was pushed.
//
// What the key cannot be is ambiguous. The machine reading this list offers to
// replace the whole document it shows, so "[]" has to mean "configured with
// none" and the absence of the key has to mean "has not said" — a node that
// announces nothing for a node that configures nothing invites a save that
// erases a working alias set.

func TestModelsHandler_OmitsAliasKeyUntilTheBrokerDeliversADocument(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt())))

	req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	response := httptest.NewRecorder()
	modelsHandler(ex)(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), "modelAliases",
		"a node whose settings service has not answered announced an alias document")
}

func TestModelsHandler_AnnouncesConfiguredAliasesUntilTheDocumentIsWithdrawn(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt())))
	ex.SetModelAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "qwen/qwen3.6-35b-a3b"}})

	req := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	response := httptest.NewRecorder()
	modelsHandler(ex)(response, req)
	require.Equal(t, http.StatusOK, response.Code)

	var announcement struct {
		ModelsResult
		ModelAliases []noderec.ModelAlias `json:"modelAliases"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &announcement))
	// A peer's scanner reads a synonym as the two names it is. No node and no
	// engine travel with it, which is what lets the same list route on the
	// machine that reads it.
	require.Len(t, announcement.ModelAliases, 1)
	require.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "qwen/qwen3.6-35b-a3b"},
		announcement.ModelAliases[0])

	// A document withdrawn — the courier sent no list at all, so nothing is
	// known — takes the key off the wire again rather than serving a stale list.
	ex.SetModelAliases(nil)
	cleared := httptest.NewRecorder()
	modelsHandler(ex)(cleared, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	require.NotContains(t, cleared.Body.String(), "modelAliases")
}

func TestModelsHandler_AnnouncesAnEmptyDocumentAsAnEmptyList(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt())))
	// The broker reads a settings service that answered "no aliases" and pushes
	// that answer as an empty list, which is a document like any other.
	ex.SetModelAliases([]noderec.ModelAlias{})

	response := httptest.NewRecorder()
	modelsHandler(ex)(response, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	require.Equal(t, http.StatusOK, response.Code)

	var announcement struct {
		ModelsResult
		ModelAliases *[]noderec.ModelAlias `json:"modelAliases"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &announcement))
	require.NotNil(t, announcement.ModelAliases,
		"an empty document must arrive as [] so it cannot be read as no answer")
	require.Empty(t, *announcement.ModelAliases)
}
