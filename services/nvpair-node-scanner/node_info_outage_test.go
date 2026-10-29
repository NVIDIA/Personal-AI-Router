// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A peer whose node-info address silently drops packets is the failure these
// cover. It cost a full request timeout on every sweep and said so only at debug
// level, so an address that had never once answered was indistinguishable in the
// log from one that was working.

// captureLogs redirects the default logger for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// Repeated polling reports outage and recovery transitions once each.
func TestNodeInfoOutage_IsStatedOnceAtEachEnd(t *testing.T) {
	logs := captureLogs(t)
	d := &daemon{}

	for range 3 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", false)
	}
	require.Equal(t, 1, strings.Count(logs.String(), "node-info is not answering"), "outage reported")
	require.Contains(t, logs.String(), "level=WARN", "an unreachable peer must be visible above debug")
	require.Contains(t, logs.String(), "10.0.0.5:14300", "the report must name where the node was asked")

	for range 2 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", true)
	}
	require.Equal(t, 1, strings.Count(logs.String(), "node-info is answering again"), "recovery reported")
}

// A peer that remains available must not produce repetitive log messages.
func TestNodeInfoOutage_EmitsNothingWhileAvailabilityIsUnchanged(t *testing.T) {
	logs := captureLogs(t)
	d := &daemon{}

	for range 5 {
		d.noteNodeInfo("peer-uuid", "10.0.0.5:14300", true)
	}
	require.Empty(t, logs.Bytes(), "a healthy peer must log nothing")
}

func TestNodeInfoOriginGate_HonorsCanceledWaiter(t *testing.T) {
	var gate nodeInfoOriginGate
	release, acquired := gate.acquire(context.Background(), "http://127.0.0.1:14318")
	require.True(t, acquired, "first origin request did not acquire the gate")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitingRelease, waitingAcquired := gate.acquire(ctx, "http://127.0.0.1:14318")
	if waitingAcquired {
		waitingRelease()
		require.FailNow(t, "canceled origin request acquired the occupied gate")
	}
}
