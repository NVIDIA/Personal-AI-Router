// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReservedAliasPortBlocksLocalAndRemoteStarts(t *testing.T) {
	manifest := testEngineManifest(fakeEngineBin)
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	exec := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	exec.overrideDir = t.TempDir()
	if err := exec.SetReservedPort(15555); err != nil {
		t.Fatal(err)
	}

	if err := exec.StartWith(context.Background(), manifest.Engine, startOpts{Port: 15555}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("local start error = %v, want reserved-port rejection", err)
	}
	if _, err := exec.SetPort(context.Background(), manifest.Engine, 15555); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("set-port error = %v, want reserved-port rejection", err)
	}

	req := httptest.NewRequest(http.MethodPost, controlStartPath, strings.NewReader(`{"engine":"fake","port":15555}`))
	rec := httptest.NewRecorder()
	(&controlServer{exec: exec}).handleStart(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "reserved") {
		t.Fatalf("remote start response = %d %q, want reserved-port rejection", rec.Code, rec.Body.String())
	}
}
