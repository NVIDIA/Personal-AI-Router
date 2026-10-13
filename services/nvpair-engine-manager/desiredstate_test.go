// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"

	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func desiredTestExecutor(t *testing.T, manifest *Manifest, baseDir string) *Executor {
	t.Helper()
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	return NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
}

func TestDesiredStateStoresExplicitOffAndLeavesLegacyUnknown(t *testing.T) {
	store := newDesiredStateStore(t.TempDir())
	enabled, known, err := store.get("fake")
	require.NoError(t, err, "legacy state (%v, %v, %v)", enabled, known, err)
	require.False(t, known, "legacy state (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "legacy state (%v, %v, %v)", enabled, known, err)
	require.NoError(t, store.set("fake", false))
	enabled, known, err = store.get("fake")
	require.NoError(t, err, "saved OFF (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "saved OFF (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "saved OFF (%v, %v, %v)", enabled, known, err)
	require.NoError(t, store.set("fake", true))
	enabled, known, err = store.get("fake")
	require.NoError(t, err, "saved ON (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "saved ON (%v, %v, %v)", enabled, known, err)
	require.True(t, enabled, "saved ON (%v, %v, %v)", enabled, known, err)
}

func TestDesiredOnSurvivesShutdownAndExplicitOffDoesNot(t *testing.T) {
	baseDir := t.TempDir()
	ctx := context.Background()

	first := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	require.NoError(t, first.Start(ctx, "fake"), "start")
	first.StopAll()

	second := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	t.Cleanup(second.StopAll)
	require.NoError(t, second.RestoreEnabled(ctx), "restore ON")
	st, err := second.Status("fake")
	require.NoError(t, err, "restored status (%v, %v)", st, err)
	require.True(t, st.Running, "restored status (%v, %v)", st, err)
	require.NoError(t, second.Stop("fake"), "explicit stop")

	third := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	require.NoError(t, third.RestoreEnabled(ctx), "restore after OFF")
	st, err = third.Status("fake")
	require.NoError(t, err, "OFF status (%v, %v)", st, err)
	require.False(t, st.Running, "OFF status (%v, %v)", st, err)
}

func TestFailedRestoreRetainsDesiredOnForRetry(t *testing.T) {
	baseDir := t.TempDir()
	seed := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	require.NoError(t, seed.Start(context.Background(), "fake"), "seed start")
	seed.StopAll()

	failing := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := failing.Platforms[key]
	platform.Runtime.Args = []string{"noserve"}
	platform.Runtime.Ready.TimeoutS = 1
	failing.Platforms[key] = platform
	require.Error(t, desiredTestExecutor(t, failing, baseDir).RestoreEnabled(context.Background()), "expected restore failure")

	retry := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	t.Cleanup(retry.StopAll)
	require.NoError(t, retry.RestoreEnabled(context.Background()), "retry restore")
	st, err := retry.Status("fake")
	require.NoError(t, err, "retry status (%v, %v)", st, err)
	require.True(t, st.Running, "retry status (%v, %v)", st, err)
}

// TestDeclinedAdoptedStopStillPersistsOffIntent proves a user Stop that is
// declined because the adopted listener is a genuinely foreign process (its
// image is not our managed binary) still records the OFF intent, so a later
// RestoreEnabled honors it rather than flipping the engine back on.
func TestDeclinedAdoptedStopStillPersistsOffIntent(t *testing.T) {
	baseDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	// The httptest listener runs in this test process, whose image is not the
	// fake engine binary, so the stop is declined (never terminated).
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	p.Runtime.Port = port
	p.Runtime.Ready = &Probe{HTTP: srv.URL, Status: http.StatusOK}
	p.Runtime.Health = nil
	m.Platforms[key] = p

	first := desiredTestExecutor(t, m, baseDir)
	st, err := first.Status("fake")
	require.NoError(t, err, "adopt foreign listener: status (%v, %v)", st, err)
	require.True(t, st.Running, "adopt foreign listener: status (%v, %v)", st, err)
	require.ErrorContains(t, first.Stop("fake"), "external management", "stop error")
	enabled, known, err := first.desired.get("fake")
	require.NoError(t, err, "desired after declined stop (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "desired after declined stop (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "desired after declined stop (%v, %v, %v)", enabled, known, err)

	srv.Close()
	fresh := desiredTestExecutor(t, m, baseDir)
	require.NoError(t, fresh.RestoreEnabled(context.Background()), "restore after declined OFF")
	st, err = fresh.Status("fake")
	require.NoError(t, err, "declined-OFF engine was restarted: status (%v, %v)", st, err)
	require.False(t, st.Running, "declined-OFF engine was restarted: status (%v, %v)", st, err)
}

func TestConfirmedStopPersistsOffDespiteCommandExitError(t *testing.T) {
	baseDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	manifest := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[key]
	platform.Runtime = Runtime{
		Mode:  "command",
		Port:  port,
		Ready: &Probe{HTTP: srv.URL, Status: http.StatusOK},
		Stop:  &StopSpec{Cmd: []string{fakeEngineBin, "failmark", marker}, GraceS: 2},
	}
	manifest.Platforms[key] = platform

	first := desiredTestExecutor(t, manifest, baseDir)
	require.NoError(t, first.Start(context.Background(), "fake"), "adopt and save ON")
	closed := make(chan struct{})
	go func() {
		for !fileExists(marker) {
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
		close(closed)
	}()
	require.NoError(t, first.Stop("fake"), "endpoint-down stop should succeed despite command exit")
	<-closed
	enabled, known, err := first.desired.get("fake")
	require.NoError(t, err, "saved state after confirmed stop (%v, %v, %v)", enabled, known, err)
	require.True(t, known, "saved state after confirmed stop (%v, %v, %v)", enabled, known, err)
	require.False(t, enabled, "saved state after confirmed stop (%v, %v, %v)", enabled, known, err)

	fresh := desiredTestExecutor(t, manifest, baseDir)
	require.NoError(t, fresh.RestoreEnabled(context.Background()), "restore after confirmed OFF")
	st, err := fresh.Status("fake")
	require.NoError(t, err, "confirmed OFF was restored unexpectedly: status (%v, %v)", st, err)
	require.False(t, st.Running, "confirmed OFF was restored unexpectedly: status (%v, %v)", st, err)
}
