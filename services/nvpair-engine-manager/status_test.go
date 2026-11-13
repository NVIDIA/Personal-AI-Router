// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// Status and GetInstalled serve UI and discovery reads while lifecycle actions
// may spend seconds in external I/O. Neither read should wait for that action.
func TestStatusReads_ReturnDuringLifecycleOperation(t *testing.T) {
	test := func(name string, read func(*Executor) (EngineStatus, error)) {
		t.Run(name, func(t *testing.T) {
			ex := newTestExecutor(t, testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt())))
			st, err := ex.state("fake")
			require.NoError(t, err)
			st.lockOperation()
			defer st.unlockOperation()

			// Run the read separately so a blocked call cannot hang the test.
			// The buffer lets it finish even if the timeout fails the test.
			finished := make(chan statusResult, 1)
			go func() {
				status, err := read(ex)
				finished <- statusResult{status, err}
			}()
			select {
			case got := <-finished:
				require.NoError(t, got.err)
				require.False(t, got.status.Running)
			case <-time.After(time.Second):
				require.FailNow(t, "status read waited for the lifecycle operation lock")
			}
		})
	}
	test("status", func(ex *Executor) (EngineStatus, error) { return ex.Status("fake") })
	test("get installed", func(ex *Executor) (EngineStatus, error) {
		statuses := ex.GetInstalled()
		if len(statuses) != 1 {
			return EngineStatus{}, fmt.Errorf("expected one engine, got %d", len(statuses))
		}
		return statuses[0], nil
	})
}

// A readiness reply can arrive after a user has stopped the engine. That old
// reply must not undo the explicit OFF intent by adopting the listener again.
func TestStatus_DoesNotReadoptAfterStopDuringProbe(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	ex := fixture.executor
	// Save the delayed status result so it can be checked after Stop.
	finished := make(chan statusResult, 1)
	go func() {
		status, err := ex.Status("fake")
		finished <- statusResult{status, err}
	}()
	awaitProbeStart(t, fixture.probeStarted)

	// Bound the wait for Stop so a lock regression fails promptly.
	stopped := make(chan error, 1)
	go func() { stopped <- ex.Stop("fake") }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		fixture.releaseProbe()
		require.FailNow(t, "Stop waited for a passive readiness probe")
	}
	fixture.releaseProbe()
	got := awaitResult(t, finished)
	require.NoError(t, got.err)
	require.False(t, got.status.Running, "an old probe re-adopted after Stop")
	status, err := ex.Status("fake")
	require.NoError(t, err)
	require.False(t, status.Running, "later status undid an explicit Stop")
}

// A failed attempt to stop a managed orphan can leave it running with stopping
// still set. A successful status check must not turn that into a health failure.
func TestStatus_DoesNotMarkReadyStoppingEngineUnhealthy(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	fixture.releaseProbe()
	st, err := fixture.executor.state("fake")
	require.NoError(t, err)
	// Reproduce the state left by a failed stop without terminating a process.
	st.mu.Lock()
	st.running = true
	st.healthy = true
	st.adopted = true
	st.stopping = true
	st.mu.Unlock()

	status, err := fixture.executor.Status("fake")
	require.NoError(t, err)
	require.True(t, status.Running)
	require.True(t, status.Healthy, "a successfully identified listener was marked unhealthy")
}

// SetPort may finish while a status check is probing the old listener. Its
// late result must not restore the port that SetPort just replaced.
func TestStatus_DoesNotRestoreOldPortAfterSetPort(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	ex := fixture.executor
	ex.overrideDir = t.TempDir()
	newPort, err := freePort()
	require.NoError(t, err)
	for newPort == fixture.port {
		newPort, err = freePort()
		require.NoError(t, err)
	}
	// Save the old-port status result for comparison after SetPort finishes.
	finished := make(chan statusResult, 1)
	go func() {
		status, err := ex.Status("fake")
		finished <- statusResult{status, err}
	}()
	awaitProbeStart(t, fixture.probeStarted)

	// Check that SetPort returns before allowing the old status reply through.
	changed := make(chan statusResult, 1)
	go func() {
		status, err := ex.SetPort(context.Background(), "fake", newPort)
		changed <- statusResult{status, err}
	}()
	select {
	case got := <-changed:
		require.NoError(t, got.err)
		require.Equal(t, newPort, got.status.Port)
	case <-time.After(time.Second):
		fixture.releaseProbe()
		require.FailNow(t, "SetPort waited for a passive readiness probe")
	}
	fixture.releaseProbe()
	got := awaitResult(t, finished)
	require.NoError(t, got.err)
	require.Equal(t, newPort, got.status.Port, "an old probe restored the former port")
	status, err := ex.Status("fake")
	require.NoError(t, err)
	require.Equal(t, newPort, status.Port)
}

// Canceling one status reader must leave the shared readiness probe alive for
// another reader, so the remaining caller still receives the probe result.
func TestStatus_SecondReaderUsesProbeAfterFirstCancels(t *testing.T) {
	fixture := newBlockedReadinessFixture(t)
	ex := fixture.executor
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Capture this caller's cancellation separately from the remaining reader.
	first := make(chan error, 1)
	go func() {
		_, err := ex.statusWithContext(ctx, "fake", 0, false)
		first <- err
	}()
	awaitProbeStart(t, fixture.probeStarted)
	// Save the other caller's result to prove that its request survives.
	second := make(chan statusResult, 1)
	go func() {
		status, err := ex.Status("fake")
		second <- statusResult{status, err}
	}()
	state, err := ex.state("fake")
	require.NoError(t, err)
	// Wait until the second reader joins before canceling the first. Without
	// this check, scheduling could make the test cancel the probe too early.
	joined := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		for _, run := range state.statusRuns {
			joined = run.waiters == 2
		}
		state.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.True(t, joined, "second reader did not join the shared probe")
	cancel()
	require.ErrorIs(t, awaitResult(t, first), context.Canceled)
	fixture.releaseProbe()
	got := awaitResult(t, second)
	require.NoError(t, got.err)
	require.True(t, got.status.Running)
	require.Equal(t, int32(1), fixture.requests.Load(), "readers started duplicate readiness requests")
}

// An explicit port check must wait for an active operation. A nil readiness
// probe keeps this case off the network, so synctest can observe the blocked
// call directly instead of guessing whether it ran during a short sleep.
func TestStatusAtPort_WaitsForActiveOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt()))
		platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
		platform.Runtime.Ready = nil
		m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
		ex := newTestExecutor(t, m)
		st, err := ex.state("fake")
		require.NoError(t, err)
		st.lockOperation()
		// Unlock even if an assertion ends the test before the normal release.
		var once sync.Once
		unlock := func() { once.Do(st.unlockOperation) }
		defer unlock()
		// Save the result while synctest waits for the caller to block or finish.
		finished := make(chan statusResult, 1)
		go func() {
			status, err := ex.StatusAtPort("fake", 12345)
			finished <- statusResult{status, err}
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Fatal("explicit port check returned before the operation finished")
		default:
		}
		unlock()
		require.NoError(t, awaitResult(t, finished).err)
	})
}

type statusResult struct {
	status EngineStatus
	err    error
}

type blockedReadinessFixture struct {
	executor      *Executor
	port          int
	probeStarted  <-chan struct{}
	probeCanceled <-chan struct{}
	releaseProbe  func()
	requests      *atomic.Int32
}

// newBlockedReadinessFixture gives a fake engine a live listener but no local
// executable, forcing Status through passive adoption. The listener holds its
// root readiness reply until released. Health polling is disabled so the tests
// control this one probe. Cleanup releases the reply before closing the server.
func newBlockedReadinessFixture(t *testing.T) blockedReadinessFixture {
	t.Helper()
	// Notify the test when a readiness request reaches the server. The buffer
	// and nonblocking send keep later requests from waiting on this signal.
	started := make(chan struct{}, 1)
	// Report when cancellation reaches the server, independently of reply release.
	canceled := make(chan struct{}, 1)
	// Hold replies while the test exercises an overlapping operation or timeout.
	// Closing the channel releases every waiting handler, including in cleanup.
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	requests := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			select {
			case canceled <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})
	port := portOf(t, srv.URL)
	m := testEngineManifest(filepath.Join(t.TempDir(), "missing"+exeExt()))
	platform := m.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	platform.Runtime.Port = port
	platform.Runtime.Health = nil
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = platform
	return blockedReadinessFixture{
		executor:      newTestExecutor(t, m),
		port:          port,
		probeStarted:  started,
		probeCanceled: canceled,
		releaseProbe:  unblock,
		requests:      requests,
	}
}

// awaitProbeStart bounds the wait for the fixture's readiness request.
func awaitProbeStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("readiness probe did not start")
	}
}

// awaitResult bounds waits on goroutines so a regression fails the test instead
// of leaving it hung. Channels are buffered or closed so senders cannot block
// after a timeout.
func awaitResult[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for operation")
		var zero T
		return zero
	}
}
