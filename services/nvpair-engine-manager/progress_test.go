// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestProgressHubFanOutAndFilter verifies a subscriber receives only its
// engine's events and that cancel closes the channel.
func TestProgressHubFanOutAndFilter(t *testing.T) {
	h := newProgressHub()
	ch, cancel := h.subscribe("ollama")

	h.publish(ProgressEvent{Engine: "lmstudio", Op: "install", Stage: "downloading", Percent: 10})
	h.publish(ProgressEvent{Engine: "ollama", Op: "install", Stage: "downloading", Percent: 25})

	select {
	case ev := <-ch:
		if ev.Engine != "ollama" || ev.Percent != 25 {
			t.Fatalf("expected ollama 25%%, got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for matching progress event")
	}

	// The non-matching (lmstudio) event must not have been delivered.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected extra event: %+v", ev)
	default:
	}

	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("expected channel closed after cancel")
	}
}

// TestProgressHubDropsWhenFull ensures a full subscriber buffer drops frames
// rather than blocking the publisher (an install must never stall on a slow
// consumer).
func TestProgressHubDropsWhenFull(t *testing.T) {
	h := newProgressHub()
	_, cancel := h.subscribe("ollama")
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			h.publish(ProgressEvent{Engine: "ollama", Op: "install", Percent: i})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a full subscriber buffer")
	}
}

// TestEmitInstallProgressPublishesToHub verifies the executor helper both
// notifies and feeds the hub.
func TestEmitInstallProgressPublishesToHub(t *testing.T) {
	var gotParams map[string]any
	e := &Executor{
		progress: newProgressHub(),
		emit: func(_ string, params any) {
			gotParams, _ = params.(map[string]any)
		},
	}
	ch, cancel := e.progress.subscribe("ollama")
	defer cancel()

	e.emitInstallProgress("ollama", "downloading", 42)

	if gotParams["stage"] != "downloading" || gotParams["percent"] != 42 {
		t.Fatalf("unexpected notification params: %+v", gotParams)
	}
	select {
	case ev := <-ch:
		if ev.Op != "install" || ev.Stage != "downloading" || ev.Percent != 42 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("emitInstallProgress did not publish to the hub")
	}
}

// TestEmitInstallProgressOmitsIndeterminatePercent verifies an unmeasurable
// install step (percent 0) reaches the wire without a percent, so the UI shows
// the stage alone instead of a made-up number.
func TestEmitInstallProgressOmitsIndeterminatePercent(t *testing.T) {
	var gotParams map[string]any
	e := &Executor{
		progress: newProgressHub(),
		emit: func(_ string, params any) {
			gotParams, _ = params.(map[string]any)
		},
	}
	ch, cancel := e.progress.subscribe("ollama")
	defer cancel()

	e.emitInstallProgress("ollama", "installing", 0)

	if gotParams["engine"] != "ollama" || gotParams["stage"] != "installing" {
		t.Fatalf("unexpected notification params: %+v", gotParams)
	}
	if _, ok := gotParams["percent"]; ok {
		t.Fatalf("indeterminate install progress carried a percent: %+v", gotParams)
	}
	select {
	case ev := <-ch:
		if ev.Stage != "installing" || ev.Percent != 0 {
			t.Fatalf("unexpected hub event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("emitInstallProgress did not publish to the hub")
	}
}

// TestInstallFlowReportsOnlyMeasurablePercents drives a full fetch+run install
// and checks that only measurable (download) and terminal frames carry a
// percent; the verified and installing steps are indeterminate.
func TestInstallFlowReportsOnlyMeasurablePercents(t *testing.T) {
	payload := []byte("engine payload")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	bin := filepath.Join(t.TempDir(), "engine"+exeExt())
	m := &Manifest{
		Engine: "fake", DisplayName: "Fake", ManifestVersion: 1,
		Platforms: map[string]Platform{hostKey(): {
			Detect: []string{bin},
			Install: &Install{
				Fetch: &Fetch{URL: srv.URL, SHA256: hex.EncodeToString(sum[:])},
				Run:   []string{fakeEngineBin, "touch", bin},
				Mode:  "user",
			},
			Runtime: Runtime{Bin: fakeEngineBin},
		}},
	}

	var mu sync.Mutex
	var frames []map[string]any
	reg := NewRegistry()
	reg.engines[m.Engine] = m
	ex := NewExecutor(reg, NewReporter(nil), func(method string, params any) {
		if method != "engine:install-progress" {
			return
		}
		p, _ := params.(map[string]any)
		mu.Lock()
		frames = append(frames, p)
		mu.Unlock()
	}, t.TempDir())
	ex.detectTimeout = 2 * time.Second

	if err := ex.Install(context.Background(), m.Engine); err != nil {
		t.Fatalf("install: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	stages := map[string]bool{}
	measuredDownload := false
	for _, f := range frames {
		stage, _ := f["stage"].(string)
		stages[stage] = true
		pct, hasPct := f["percent"]
		switch stage {
		case "downloading":
			if hasPct {
				if p, _ := pct.(int); p <= 0 {
					t.Errorf("downloading frame carried a non-positive percent: %+v", f)
				}
				measuredDownload = true
			}
		case "verified", "installing":
			if hasPct {
				t.Errorf("%s frame carried a percent: %+v", stage, f)
			}
		case "done":
			if pct != 100 {
				t.Errorf("done frame percent = %v, want 100", pct)
			}
		default:
			t.Errorf("unexpected install stage %q: %+v", stage, f)
		}
	}
	if !measuredDownload {
		t.Errorf("no downloading frame carried a measured percent; got %+v", frames)
	}
	for _, want := range []string{"downloading", "verified", "installing", "done"} {
		if !stages[want] {
			t.Errorf("missing %q frame; got %+v", want, frames)
		}
	}
}

// TestEmitPullProgressNotifiesAndPublishes verifies the pull helper emits the
// local engine:pull-progress notification (with op/message) and also feeds the
// hub, so a local pull surfaces progress like a remote pull.
func TestEmitPullProgressNotifiesAndPublishes(t *testing.T) {
	var gotMethod string
	var gotParams map[string]any
	e := &Executor{
		progress: newProgressHub(),
		emit: func(method string, params any) {
			gotMethod = method
			gotParams, _ = params.(map[string]any)
		},
	}
	ch, cancel := e.progress.subscribe("ollama")
	defer cancel()

	e.emitPullProgress(ProgressEvent{Engine: "ollama", Op: "pull", Stage: "pulling", Percent: 62, Message: "pulling"})

	if gotMethod != "engine:pull-progress" {
		t.Fatalf("expected engine:pull-progress notification, got %q", gotMethod)
	}
	if gotParams["engine"] != "ollama" || gotParams["op"] != "pull" || gotParams["percent"] != 62 || gotParams["message"] != "pulling" {
		t.Fatalf("unexpected notification params: %+v", gotParams)
	}

	select {
	case ev := <-ch:
		if ev.Op != "pull" || ev.Stage != "pulling" || ev.Percent != 62 {
			t.Fatalf("unexpected hub event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("emitPullProgress did not publish to the hub")
	}
}
