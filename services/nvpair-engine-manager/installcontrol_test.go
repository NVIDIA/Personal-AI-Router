// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEngineManager_ReportsLMStudioDownloadTimeoutOverStdio(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "curl: (28) Operation timed out\nline 228: _checksum_value: unbound variable", 10)
	// Give the real worker a private port and an absolute detect path, so it
	// cannot adopt or modify an engine installed on the developer's machine.
	p := f.manifest.Platforms[hostKey()]
	port, err := freePort()
	require.NoError(t, err)
	p.Runtime.Port = port
	p.Runtime.Mode = "process"
	f.manifest.Platforms[hostKey()] = p
	cfg, home := t.TempDir(), t.TempDir()
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, writeJSONAtomic(filepath.Join(dir, "lmstudio.json"), f.manifest))
	}
	manager := startE2EManager(t, cfg, home)
	send(t, manager.stdin, 1, "engine:install", map[string]string{"engine": "lmstudio"})
	want := "LM Studio download timed out after 3 attempts. Check your connection and try again."
	installing, failed, reported := 0, 0, 0
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame := <-manager.frames:
			switch frame.Method {
			case "engine:install-progress":
				var progress struct{ Engine, Stage, Error string }
				require.NoError(t, json.Unmarshal(frame.Params, &progress))
				if progress.Engine != "lmstudio" {
					continue
				}
				switch progress.Stage {
				case "installing":
					installing++
				case "failed":
					failed++
					require.Equal(t, 3, installing, "failure must follow all attempts")
					require.Equal(t, want, progress.Error)
				}
			case "errors:report":
				var entry serviceError
				require.NoError(t, json.Unmarshal(frame.Params, &entry))
				if entry.ID == installFailedID("lmstudio") {
					reported++
					require.Equal(t, 3, installing)
					require.Equal(t, want, entry.Message)
					require.Equal(t, "retry", entry.Action)
				}
			}
			if string(frame.ID) == "1" {
				var rpcError RPCError
				require.NoError(t, json.Unmarshal(frame.Error, &rpcError))
				require.Equal(t, -32000, rpcError.Code)
				require.Equal(t, want, rpcError.Message)
				require.Equal(t, 1, failed)
				require.Equal(t, 1, reported)
				require.Equal(t, 3, readCount(t, f.counter))
				require.Equal(t, int32(3), f.requests.Load())
				manager.stop(t)
				return
			}
		case <-deadline.C:
			t.Fatal("LM Studio install did not return its terminal RPC error")
		}
	}
}

func TestRemoteInstall_StreamsOneFailureAfterLMStudioRetries(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "line 213: _checksum_value: unbound variable", 10)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, controlInstallPath, strings.NewReader(`{"opId":"install-test","engine":"lmstudio"}`))
	(&controlServer{exec: f.ex}).handleInstall(recorder, request)
	frames := decodeFrames(t, recorder.Body.String())
	require.NotEmpty(t, frames)
	want := "LM Studio’s installer could not download its checksum after 3 attempts. Check your connection and try again."
	installing, failed, terminal := 0, 0, 0
	for _, frame := range frames {
		require.Equal(t, "install-test", frame.OpID)
		require.Equal(t, "lmstudio", frame.Engine)
		require.Equal(t, "install", frame.Op)
		if frame.Type == "progress" {
			switch frame.Stage {
			case "installing":
				installing++
			case "failed":
				failed++
				require.Equal(t, 3, installing)
				require.Equal(t, want, frame.Message)
			}
		} else {
			terminal++
			require.Equal(t, "error", frame.Type)
			require.Equal(t, want, frame.Message)
		}
	}
	require.Equal(t, "error", frames[len(frames)-1].Type)
	require.Equal(t, 1, terminal)
	require.Equal(t, 1, failed)
	require.Equal(t, 3, readCount(t, f.counter))
	require.Equal(t, want, f.ex.reporter.snapshot()[0].Message)
}

func TestRemoteInstall_StreamsSuccessAfterLMStudioRecovers(t *testing.T) {
	f := newInstallFixture(t, "lmstudio", "curl: (28) Operation timed out", 2)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, controlInstallPath, strings.NewReader(`{"opId":"install-test","engine":"lmstudio"}`))
	(&controlServer{exec: f.ex}).handleInstall(recorder, request)
	frames := decodeFrames(t, recorder.Body.String())
	require.NotEmpty(t, frames)
	terminal := frames[len(frames)-1]
	require.Equal(t, "result", terminal.Type)
	require.NotNil(t, terminal.Status)
	require.True(t, terminal.Status.Installed)
	results, done := 0, 0
	for _, frame := range frames {
		require.NotEqual(t, "error", frame.Type)
		require.NotEqual(t, "failed", frame.Stage)
		if frame.Type == "result" {
			results++
		}
		if frame.Stage == "done" {
			done++
		}
	}
	require.Equal(t, 1, results)
	require.Equal(t, 1, done)
	require.Empty(t, f.ex.reporter.snapshot())
	require.Equal(t, 3, readCount(t, f.counter))
}
