// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
	e := &Executor{progress: newProgressHub()}
	ch, cancel := e.progress.subscribe("ollama")
	defer cancel()

	e.emitInstallProgress("ollama", "installing", 75)

	select {
	case ev := <-ch:
		if ev.Op != "install" || ev.Stage != "installing" || ev.Percent != 75 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("emitInstallProgress did not publish to the hub")
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
