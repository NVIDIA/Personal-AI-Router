// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReservedAliasPortBlocksLocalAndRemoteStarts(t *testing.T) {
	manifest := testEngineManifest(fakeEngineBin)
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	exec := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	exec.overrideDir = t.TempDir()
	require.NoError(t, exec.SetReservedPort(15555))

	{
		err := exec.StartWith(context.Background(), manifest.Engine, startOpts{Port: 15555})
		require.Error(t, err, "local start error")
		require.Contains(t, err.Error(), "reserved", "local start error (%v)", err)
	}
	{
		_, err := exec.SetPort(context.Background(), manifest.Engine, 15555)
		require.Error(t, err, "set-port error")
		require.Contains(t, err.Error(), "reserved", "set-port error (%v)", err)
	}

	req := httptest.NewRequest(http.MethodPost, controlStartPath, strings.NewReader(`{"engine":"fake","port":15555}`))
	rec := httptest.NewRecorder()
	(&controlServer{exec: exec}).handleStart(rec, req)
	require.True(t, rec.Code == http.StatusInternalServerError, "remote start response")
	require.Contains(t, rec.Body.String(), "reserved", "remote start response")
}
