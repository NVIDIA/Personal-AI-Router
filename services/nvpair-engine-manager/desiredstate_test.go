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
	"strings"
	"testing"
	"time"
)

func desiredTestExecutor(t *testing.T, manifest *Manifest, baseDir string) *Executor {
	t.Helper()
	reg := NewRegistry()
	reg.engines[manifest.Engine] = manifest
	return NewExecutor(reg, NewReporter(nil), func(string, any) {}, baseDir)
}

func TestDesiredStateStoresExplicitOffAndLeavesLegacyUnknown(t *testing.T) {
	store := newDesiredStateStore(t.TempDir())
	if enabled, known, err := store.get("fake"); err != nil || known || enabled {
		t.Fatalf("legacy state = (%v, %v, %v), want unknown", enabled, known, err)
	}
	if err := store.set("fake", false); err != nil {
		t.Fatal(err)
	}
	if enabled, known, err := store.get("fake"); err != nil || !known || enabled {
		t.Fatalf("saved OFF = (%v, %v, %v), want known false", enabled, known, err)
	}
	if err := store.set("fake", true); err != nil {
		t.Fatal(err)
	}
	if enabled, known, err := store.get("fake"); err != nil || !known || !enabled {
		t.Fatalf("saved ON = (%v, %v, %v), want known true", enabled, known, err)
	}
}

func TestDesiredOnSurvivesShutdownAndExplicitOffDoesNot(t *testing.T) {
	baseDir := t.TempDir()
	ctx := context.Background()

	first := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	if err := first.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	first.StopAll()

	second := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	t.Cleanup(second.StopAll)
	if err := second.RestoreEnabled(ctx); err != nil {
		t.Fatalf("restore ON: %v", err)
	}
	if st, err := second.Status("fake"); err != nil || !st.Running {
		t.Fatalf("restored status = %+v, err=%v", st, err)
	}
	if err := second.Stop("fake"); err != nil {
		t.Fatalf("explicit stop: %v", err)
	}

	third := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	if err := third.RestoreEnabled(ctx); err != nil {
		t.Fatalf("restore after OFF: %v", err)
	}
	if st, err := third.Status("fake"); err != nil || st.Running {
		t.Fatalf("OFF status = %+v, err=%v", st, err)
	}
}

func TestFailedRestoreRetainsDesiredOnForRetry(t *testing.T) {
	baseDir := t.TempDir()
	seed := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	if err := seed.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("seed start: %v", err)
	}
	seed.StopAll()

	failing := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	platform := failing.Platforms[key]
	platform.Runtime.Args = []string{"noserve"}
	platform.Runtime.Ready.TimeoutS = 1
	failing.Platforms[key] = platform
	if err := desiredTestExecutor(t, failing, baseDir).RestoreEnabled(context.Background()); err == nil {
		t.Fatal("expected restore failure")
	}

	retry := desiredTestExecutor(t, testEngineManifest(fakeEngineBin), baseDir)
	t.Cleanup(retry.StopAll)
	if err := retry.RestoreEnabled(context.Background()); err != nil {
		t.Fatalf("retry restore: %v", err)
	}
	if st, err := retry.Status("fake"); err != nil || !st.Running {
		t.Fatalf("retry status = %+v, err=%v (state file %s)", st, err, filepath.Join(baseDir, desiredStateFileName))
	}
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
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

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
	if st, err := first.Status("fake"); err != nil || !st.Running {
		t.Fatalf("adopt foreign listener: status=%+v err=%v", st, err)
	}
	if err := first.Stop("fake"); err == nil || !strings.Contains(err.Error(), "external management") {
		t.Fatalf("stop error = %v, want external-management decline", err)
	}
	if enabled, known, err := first.desired.get("fake"); err != nil || !known || enabled {
		t.Fatalf("desired after declined stop = (%v, %v, %v), want known OFF", enabled, known, err)
	}

	srv.Close()
	fresh := desiredTestExecutor(t, m, baseDir)
	if err := fresh.RestoreEnabled(context.Background()); err != nil {
		t.Fatalf("restore after declined OFF: %v", err)
	}
	if st, err := fresh.Status("fake"); err != nil || st.Running {
		t.Fatalf("declined-OFF engine was restarted: status=%+v err=%v", st, err)
	}
}

func TestConfirmedStopPersistsOffDespiteCommandExitError(t *testing.T) {
	baseDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "stop-attempted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

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
	if err := first.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("adopt and save ON: %v", err)
	}
	closed := make(chan struct{})
	go func() {
		for !fileExists(marker) {
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
		close(closed)
	}()
	if err := first.Stop("fake"); err != nil {
		t.Fatalf("endpoint-down stop should succeed despite command exit: %v", err)
	}
	<-closed
	if enabled, known, err := first.desired.get("fake"); err != nil || !known || enabled {
		t.Fatalf("saved state after confirmed stop = (%v, %v, %v), want known OFF", enabled, known, err)
	}

	fresh := desiredTestExecutor(t, manifest, baseDir)
	if err := fresh.RestoreEnabled(context.Background()); err != nil {
		t.Fatalf("restore after confirmed OFF: %v", err)
	}
	if st, err := fresh.Status("fake"); err != nil || st.Running {
		t.Fatalf("confirmed OFF was restored unexpectedly: status=%+v err=%v", st, err)
	}
}
