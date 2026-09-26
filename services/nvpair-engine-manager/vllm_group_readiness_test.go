// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type managedReadinessTransport func(*http.Request) (*http.Response, error)

func (f managedReadinessTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type managedReadinessFixture struct {
	rank        *vllmManagedRank
	binding     vllmGroupBinding
	posts       atomic.Int32
	gets        atomic.Int32
	owns        atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	processDone chan struct{}
	releaseOnce sync.Once
	closeOnce   sync.Once
	fail        bool
}

func readinessFixture(t *testing.T, fail bool) *managedReadinessFixture {
	t.Helper()
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	b, p := managedRankFixture(t, 0)
	h := &managedReadinessFixture{binding: b, entered: make(chan struct{}), release: make(chan struct{}), processDone: make(chan struct{}), fail: fail}
	h.owns.Store(true)
	proc := &managedProc{done: h.processDone}
	h.rank = &vllmManagedRank{e: f.e, st: f.st, binding: b, placement: p, path: filepath.Join(f.st.installDir, "rank.json"), proc: proc, receipt: vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Rank: b.Rank, State: "started"}}
	f.st.proc = proc
	f.st.running = true
	f.st.servingModel = b.Plan.Model
	f.st.version = b.Plan.Runtime
	f.st.port = p.APIPort
	f.e.vllmOwnsListener = func(*managedProc, int) bool { return h.owns.Load() }
	f.e.client = &http.Client{Transport: managedReadinessTransport(func(req *http.Request) (*http.Response, error) {
		body := "{}"
		status := 200
		if req.Method == http.MethodPost {
			requestBody, _ := io.ReadAll(req.Body)
			if req.URL.Path != "/v1/chat/completions" || !bytes.Contains(requestBody, []byte(`"messages"`)) || bytes.Contains(requestBody, []byte(`"prompt"`)) {
				return nil, errors.New("qualification did not use the bounded chat-completions contract")
			}
			if h.posts.Add(1) == 1 {
				close(h.entered)
			}
			select {
			case <-h.release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			body = fmt.Sprintf(`{"model":%q,"choices":[{}]}`, b.Plan.Model)
			if fail {
				status = 500
			}
		} else {
			h.gets.Add(1)
			switch req.URL.Path {
			case "/version":
				body = fmt.Sprintf(`{"version":%q}`, b.Plan.Runtime)
			case "/v1/models":
				body = fmt.Sprintf(`{"object":"list","data":[{"id":%q,"owned_by":"vllm"}]}`, b.Plan.Model)
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { h.releaseOnce.Do(func() { close(h.release) }); h.closeOnce.Do(func() { close(h.processDone) }) })
	return h
}
func waitReadinessQualification(t *testing.T, h *managedReadinessFixture) {
	t.Helper()
	h.rank.mu.Lock()
	done := h.rank.qualificationDone
	h.rank.mu.Unlock()
	if done == nil {
		t.Fatal("qualification did not start")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("qualification did not finish")
	}
}

func TestVLLMManagedReadinessPendingThenOneQualification(t *testing.T) {
	h := readinessFixture(t, false)
	request, cancel := context.WithCancel(context.Background())
	state, err := h.rank.readiness(request, h.binding)
	if err != nil || state != "started" {
		t.Fatalf("first short reply = %q %v", state, err)
	}
	cancel() // Closing the peer request must not cancel the rank's own qualification.
	select {
	case <-h.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("generation qualification did not start")
	}
	for range 20 {
		if err := h.rank.ready(context.Background(), h.binding); !errors.Is(err, errVLLMRankReadinessPending) {
			t.Fatalf("expected explicit pending: %v", err)
		}
	}
	if h.posts.Load() != 1 {
		t.Fatal("pending polls repeated inference")
	}
	h.releaseOnce.Do(func() { close(h.release) })
	waitReadinessQualification(t, h)
	before := h.gets.Load()
	for range 8 {
		if err := h.rank.ready(context.Background(), h.binding); err != nil {
			t.Fatal(err)
		}
	}
	if h.posts.Load() != 1 || h.gets.Load() <= before {
		t.Fatal("qualified polling inferred again or skipped current health")
	}
	h.owns.Store(false)
	if err := h.rank.ready(context.Background(), h.binding); err == nil || errors.Is(err, errVLLMRankReadinessPending) {
		t.Fatal("lost ownership was converted into pending")
	}
}
func TestVLLMManagedReadinessQualificationFailureIsTerminal(t *testing.T) {
	h := readinessFixture(t, true)
	if err := h.rank.ready(context.Background(), h.binding); !errors.Is(err, errVLLMRankReadinessPending) {
		t.Fatal(err)
	}
	h.releaseOnce.Do(func() { close(h.release) })
	waitReadinessQualification(t, h)
	for range 3 {
		if err := h.rank.ready(context.Background(), h.binding); err == nil || errors.Is(err, errVLLMRankReadinessPending) {
			t.Fatal("failure became pending or success")
		}
	}
	if h.posts.Load() != 1 {
		t.Fatal("failed qualification retried")
	}
	if err := h.rank.call(context.Background(), h.binding, "status"); err == nil {
		t.Fatal("failed rank remained live to supervisor")
	}
}
func TestVLLMManagedReadinessStopCancelsQualification(t *testing.T) {
	h := readinessFixture(t, false)
	if err := h.rank.ready(context.Background(), h.binding); !errors.Is(err, errVLLMRankReadinessPending) {
		t.Fatal(err)
	}
	select {
	case <-h.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("qualification did not start")
	}
	// Harmless owned-process fixture is already exited; no OS signal is sent.
	h.closeOnce.Do(func() { close(h.processDone) })
	if err := h.rank.stop(h.binding); err != nil {
		t.Fatal(err)
	}
	waitReadinessQualification(t, h)
	if h.rank.receipt.State != "stopped" || !h.rank.receipt.CleanupConfirmed || h.rank.st.healthy {
		t.Fatal("late qualification republished a stopped owner")
	}
}
