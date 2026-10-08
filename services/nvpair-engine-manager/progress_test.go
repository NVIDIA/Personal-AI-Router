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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.Equal(t, "ollama", ev.Engine, "expected ollama 25 (%v)", ev)
		require.Equal(t, 25, ev.Percent, "expected ollama 25 (%v)", ev)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for matching progress event")
	}

	// The non-matching (lmstudio) event must not have been delivered.
	select {
	case ev := <-ch:
		require.FailNowf(t, "unexpected extra event", "%+v", ev)
	default:
	}

	cancel()
	_, ok := <-ch
	require.False(t, ok, "expected channel closed after cancel")
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
		require.FailNow(t, "publish blocked on a full subscriber buffer")
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

	require.Equal(t, "downloading", gotParams["stage"])
	require.Equal(t, 42, gotParams["percent"])
	select {
	case ev := <-ch:
		require.Equal(t, "install", ev.Op, "unexpected event (%v)", ev)
		require.Equal(t, "downloading", ev.Stage, "unexpected event (%v)", ev)
		require.Equal(t, 42, ev.Percent, "unexpected event (%v)", ev)
	case <-time.After(time.Second):
		require.FailNow(t, "emitInstallProgress did not publish to the hub")
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

	require.Equal(t, "ollama", gotParams["engine"])
	require.Equal(t, "installing", gotParams["stage"])
	require.NotContains(t, gotParams, "percent", "indeterminate install progress carried a percent")
	select {
	case ev := <-ch:
		require.Equal(t, "installing", ev.Stage)
		require.Zero(t, ev.Percent)
	case <-time.After(time.Second):
		require.FailNow(t, "emitInstallProgress did not publish to the hub")
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

	require.NoError(t, ex.Install(context.Background(), m.Engine), "install")

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
				p, _ := pct.(int)
				assert.Greater(t, p, 0, "downloading frame must carry a positive percent")
				measuredDownload = true
			}
		case "verified", "installing":
			assert.NotContains(t, f, "percent", "%s frame carried a percent", stage)
		case "done":
			assert.Equal(t, 100, pct, "done frame percent")
		default:
			assert.Failf(t, "unexpected install stage", "%q: %+v", stage, f)
		}
	}
	assert.True(t, measuredDownload, "no downloading frame carried a measured percent; frames: %+v", frames)
	for _, want := range []string{"downloading", "verified", "installing", "done"} {
		assert.Contains(t, stages, want, "missing install progress stage; frames: %+v", frames)
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

	require.Equal(t, "engine:pull-progress", gotMethod, "expected engine:pull-progress notification")
	require.Equal(t, "ollama", gotParams["engine"], "unexpected notification params (%v)", gotParams)
	require.Equal(t, "pull", gotParams["op"], "unexpected notification params (%v)", gotParams)
	require.Equal(t, 62, gotParams["percent"], "unexpected notification params (%v)", gotParams)
	require.Equal(t, "pulling", gotParams["message"], "unexpected notification params (%v)", gotParams)

	select {
	case ev := <-ch:
		require.Equal(t, "pull", ev.Op, "unexpected hub event (%v)", ev)
		require.Equal(t, "pulling", ev.Stage, "unexpected hub event (%v)", ev)
		require.Equal(t, 62, ev.Percent, "unexpected hub event (%v)", ev)
	case <-time.After(time.Second):
		require.FailNow(t, "emitPullProgress did not publish to the hub")
	}
}
