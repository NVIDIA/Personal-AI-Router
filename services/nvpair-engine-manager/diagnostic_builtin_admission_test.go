// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func builtinDiagnosticFixture(t *testing.T, reserved bool) *Executor {
	t.Helper()
	e := NewExecutor(nil, NewReporter(nil), nil, t.TempDir())
	e.diagnostics = &diagnosticService{m: &Manager{exec: e}, recoveryFailed: reserved}
	e.client = &http.Client{Transport: actionBodyTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"version":"fixture"}`)), Request: r}, nil
	})}
	return e
}

func TestDiagnosticBuiltinMutationAdmissionPrecedesEffects(t *testing.T) {
	for _, engine := range []string{"llamacpp", "vllm"} {
		for _, action := range []string{"update", "pull_model", "import_model", "delete_model", "load_model"} {
			t.Run(engine+"/"+action, func(t *testing.T) {
				e := builtinDiagnosticFixture(t, true)
				var ioCalls atomic.Int32
				e.client = &http.Client{Transport: actionBodyTransport(func(*http.Request) (*http.Response, error) {
					ioCalls.Add(1)
					return nil, fmt.Errorf("unexpected transport")
				})}
				e.vllmExec = func(context.Context, string, []string, []string) ([]byte, error) {
					ioCalls.Add(1)
					return nil, fmt.Errorf("unexpected command")
				}
				builtin := "vllm"
				if engine == "llamacpp" {
					builtin = "llama-models"
				}
				st := &engineState{manifest: &Manifest{Engine: engine, Actions: map[string]Action{action: {Builtin: builtin}}}, installDir: filepath.Join(e.baseDir, engine)}
				e.engines[engine] = st
				params := json.RawMessage(`{"model":"org/model","operationId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
				var raw json.RawMessage
				var err error
				if action == "pull_model" {
					raw, err = e.PullModelStream(context.Background(), engine, "org/model", params)
				} else {
					raw, err = e.Action(context.Background(), engine, action, params)
				}
				if err == nil || !strings.Contains(err.Error(), "collective diagnostic reservation") || raw != nil {
					t.Fatalf("builtin bypassed admission: result=%s err=%v", raw, err)
				}
				if st.diagnosticAdmitted || st.mutationCancel != nil || st.pullCancel != nil {
					t.Fatal("refused builtin published mutation ownership")
				}
				if _, err := os.Stat(st.installDir); !os.IsNotExist(err) || ioCalls.Load() != 0 {
					t.Fatal("refused builtin reached filesystem, transport or command effects")
				}
			})
		}
	}
}

func TestDiagnosticAdmissionNestedStartAndDispatchWhileWriterWaits(t *testing.T) {
	e := builtinDiagnosticFixture(t, false)
	st := &engineState{running: true, healthy: true, port: 8082}
	outerReady, runNested, releaseOuter := make(chan struct{}), make(chan struct{}), make(chan struct{})
	nestedDone := make(chan error, 1)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		st.opMu.Lock()
		defer st.opMu.Unlock()
		_, finish, err := e.beginVLLMMutation(context.Background(), st)
		if err != nil {
			nestedDone <- err
			close(outerReady)
			return
		}
		defer finish()
		close(outerReady)
		<-runNested
		// Rollback paths use a fresh context while retaining the original opMu.
		err = e.doStart(context.Background(), st, "vllm", startOpts{})
		if err == nil {
			_, err = e.dispatchActionAdmitted(context.Background(), st, "llamacpp", "refresh_models", Action{HTTP: &ActionHTTP{Method: http.MethodGet, Path: "/models?reload=1"}}, nil)
		}
		nestedDone <- err
		<-releaseOuter
	}()
	<-outerReady
	writerDone := make(chan struct{})
	go func() {
		e.diagnosticMu.Lock()
		e.diagnostics.mu.Lock()
		e.diagnostics.recoveryFailed = true
		e.diagnostics.mu.Unlock()
		e.diagnosticMu.Unlock()
		close(writerDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for e.diagnosticMu.TryRLock() {
		e.diagnosticMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("diagnostic writer never queued")
		}
		runtime.Gosched()
	}
	close(runNested)
	select {
	case err := <-nestedDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nested start/dispatch reacquired a read lock behind the writer")
	}
	select {
	case <-writerDone:
		t.Fatal("writer entered before the original mutation released admission")
	default:
	}
	close(releaseOuter)
	<-ownerDone
	<-writerDone
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if st.diagnosticAdmitted {
		t.Fatal("admission survived the original guard")
	}
	if err := e.doStart(context.Background(), st, "vllm", startOpts{}); err == nil || !strings.Contains(err.Error(), "collective diagnostic reservation") {
		t.Fatalf("new work reused released admission: %v", err)
	}
}

func TestDiagnosticWriterDoesNotBlockBuiltinCancelOrRead(t *testing.T) {
	e := builtinDiagnosticFixture(t, true)
	var canceled atomic.Int32
	for _, engine := range []string{"llamacpp", "vllm"} {
		builtin := "vllm"
		versionAction := Action{Builtin: "vllm"}
		if engine == "llamacpp" {
			builtin = "llama-models"
			versionAction = Action{HTTP: &ActionHTTP{Method: http.MethodGet, Path: "/version"}}
		}
		e.engines[engine] = &engineState{running: true, port: 8082, installDir: filepath.Join(e.baseDir, engine), pullCancel: func() { canceled.Add(1) }, pullModel: "org/model", pullOperationID: strings.Repeat("a", 32), manifest: &Manifest{Engine: engine, Actions: map[string]Action{
			"cancel_pull":           {Builtin: builtin},
			"get_version":           versionAction,
			"get_resource_settings": {Builtin: "vllm"},
		}}}
	}
	e.diagnosticMu.Lock()
	defer e.diagnosticMu.Unlock()
	done := make(chan error, 1)
	go func() {
		params := json.RawMessage(`{"model":"org/model","operationId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		for _, engine := range []string{"llamacpp", "vllm"} {
			if _, err := e.Action(context.Background(), engine, "cancel_pull", params); err != nil {
				done <- err
				return
			}
			if _, err := e.Action(context.Background(), engine, "get_version", nil); err != nil {
				done <- err
				return
			}
		}
		if _, err := e.Action(context.Background(), "vllm", "get_resource_settings", nil); err != nil {
			done <- err
			return
		}
		if canceled.Load() != 2 {
			done <- fmt.Errorf("matched cancellation count = %d", canceled.Load())
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic writer blocked exact cancellation or read-only state")
	}
}

func TestDiagnosticAdmissionCanceledMutationReleasesGuard(t *testing.T) {
	e := builtinDiagnosticFixture(t, false)
	st := &engineState{}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.beginVLLMMutation(ctx, st); err != context.Canceled {
		t.Fatalf("canceled mutation: %v", err)
	}
	if st.diagnosticAdmitted {
		t.Fatal("canceled mutation retained admission")
	}
	if !e.diagnosticMu.TryLock() {
		t.Fatal("canceled mutation retained the read lock")
	}
	e.diagnosticMu.Unlock()
}
