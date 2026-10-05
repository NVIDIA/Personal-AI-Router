// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// closeRecorder is an io.Closer that records whether Close was called.
type closeRecorder struct {
	mu     sync.Mutex
	closed bool
}

func (c *closeRecorder) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *closeRecorder) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// TestWaitForStdinCloseWaitsForOrderlyExit covers the ordinary path: signal
// shutdown by closing stdin, then let the worker finish on its own. Nothing may
// be signalled while the worker is still inside its grace.
func TestWaitForStdinCloseWaitsForOrderlyExit(t *testing.T) {
	resetTeardownClock()
	stdin := &closeRecorder{}
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		waitForStdinClose("scanner", nil, stdin, done)
		close(returned)
	}()

	select {
	case <-returned:
		require.FailNow(t, "test expectation failed", "returned before the worker exited")
	case <-time.After(200 * time.Millisecond):
	}
	require.True(t, stdin.wasClosed(), "did not close stdin to signal shutdown")

	close(done)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "test expectation failed", "did not return after the worker exited")
	}
}

// TestWaitForStdinCloseGivesUpWithoutProcess is the anti-deadlock guarantee. A
// worker that never exits must not park teardown forever: before this was
// bounded, a worker blocked in write() on a full stderr pipe held the broker here
// until it was force-killed from outside, skipping its remaining shutdown work.
func TestWaitForStdinCloseGivesUpWithoutProcess(t *testing.T) {
	resetTeardownClock()
	stdin := &closeRecorder{}
	done := make(chan struct{})
	defer close(done)

	returned := make(chan struct{})
	go func() {
		waitForStdinClose("scanner", nil, stdin, done)
		close(returned)
	}()

	// No process handle to escalate to, so it must abandon the join after the
	// grace rather than waiting on an exit that is never coming.
	select {
	case <-returned:
	case <-time.After(workerStopGrace + 3*time.Second):
		require.FailNow(t, "test expectation failed", "never returned for a worker that does not exit; teardown can still deadlock")
	}
}

// TestWaitForStdinCloseKillsWorkerThatIgnoresShutdown checks the escalation
// actually reaches a real process that will not leave on its own.
func TestWaitForStdinCloseKillsWorkerThatIgnoresShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to spawn a signal-ignoring sleeper")
	}
	resetTeardownClock()

	// Ignore SIGTERM so the terminate step cannot be what ends it, forcing the
	// kill step to be exercised.
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; sleep 120")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "stdin pipe")
	require.NoError(t, cmd.Start(), "start")

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	returned := make(chan struct{})
	go func() {
		waitForStdinClose("scanner", cmd, stdin, done)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(workerStopGrace + 4*workerSignalGrace + 5*time.Second):
		_ = cmd.Process.Kill()
		require.FailNow(t, "test expectation failed", "escalation never completed for a worker that ignores shutdown")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		require.FailNow(t, "test expectation failed", "worker was left running after the join returned")
	}
}

// TestTeardownBudgetCapsAggregateWait is the guarantee that per-worker graces
// cannot add up. The joins run sequentially, so eleven workers that each hang for
// their own grace would take far longer than the parent waits before killing this
// process — and being killed mid-teardown skips the rest of shutdown.
func TestTeardownBudgetCapsAggregateWait(t *testing.T) {
	resetTeardownClock()
	beginTeardown()

	// Never closed: every worker hangs, the worst case.
	done := make(chan struct{})
	defer close(done)

	const workers = 11
	started := time.Now()
	for i := 0; i < workers; i++ {
		waitForStdinClose("scanner", nil, &closeRecorder{}, done)
	}
	elapsed := time.Since(started)

	unbounded := workers * workerStopGrace
	require.Less(t, elapsed, unbounded, "aggregate wait")
	// Budget, plus the floor each worker keeps once it is spent, plus slack.
	ceiling := teardownBudget + workers*minWorkerGrace + 2*time.Second
	require.LessOrEqual(t, elapsed, ceiling, "aggregate wait")
}

// TestGraceForClipsToRemainingBudget checks a worker still gets its own grace when
// the budget is untouched, and is clipped rather than starved once it is spent.
// TestGraceForIgnoresBudgetOutsideTeardown is the regression guard for a
// process-global clock armed by the wrong join. supervisor.Restart stops a worker
// at ordinary runtime — LM Studio port reconciliation does it on every rebind — and
// arming the budget there left it permanently spent, so the real shutdown gave
// every worker the floor and signalled engine-manager almost immediately.
func TestGraceForIgnoresBudgetOutsideTeardown(t *testing.T) {
	resetTeardownClock()

	// A restart-shaped stop, long before any shutdown. It must not be clipped, and
	// it must not arm anything.
	{
		got := graceFor("lmstudio-proxy")
		require.True(t, got == workerStopGrace, "restart-path grace (%v, %v)", got, workerStopGrace)
	}
	stdin := &closeRecorder{}
	done := make(chan struct{})
	close(done)
	waitForStdinClose("lmstudio-proxy", nil, stdin, done)

	// The later real teardown must still start with a full budget.
	beginTeardown()
	{
		got := graceFor(engineManagerWorkerName)
		require.True(t, got == engineManagerStopGrace, "engine-manager grace after an earlier restart (%v, %v)", got, engineManagerStopGrace)
	}
}

// TestGraceForReservesEngineManagerAllowance covers join order: engine-manager is
// joined eighth of eleven, so without a reservation the workers ahead of it decide
// how long it gets to stop its engines.
func TestGraceForReservesEngineManagerAllowance(t *testing.T) {
	// Earlier joins have spent the budget down to just over engine-manager's
	// reserve, so an ordinary worker is left with less than the floor.
	setTeardownStart(time.Now().Add(-(teardownBudget - engineManagerStopGrace - 100*time.Millisecond)))

	{
		got := graceFor("scanner")
		require.True(t, got == minWorkerGrace, "ordinary worker grace (%v, %v)", got, minWorkerGrace)
	}
	// Its reserve survives what the others spent, give or take the clock ticking
	// between these two calls.
	{
		got := graceFor(engineManagerWorkerName)
		require.GreaterOrEqual(t, got, engineManagerStopGrace-50*time.Millisecond, "engine-manager grace (%v, %v)", got, engineManagerStopGrace)
	}
}

// TestEngineShutdownDeadlineDrawsOnTheBudget checks the one step that used to have
// no deadline at all cannot spend the whole window before a worker is joined.
func TestEngineShutdownDeadlineDrawsOnTheBudget(t *testing.T) {
	resetTeardownClock()
	{
		got := engineShutdownDeadline()
		require.True(t, got == engineStopAllBudget, "deadline outside teardown (%v, %v)", got, engineStopAllBudget)
	}

	setTeardownStart(time.Now().Add(-(teardownBudget - time.Second)))
	got := engineShutdownDeadline()
	require.LessOrEqual(t, got, time.Second, "deadline with 1s of budget left")
	require.GreaterOrEqual(t, got, minWorkerGrace, "deadline with 1s of budget left")

	setTeardownStart(time.Now().Add(-2 * teardownBudget))
	{
		got := engineShutdownDeadline()
		require.True(t, got == minWorkerGrace, "deadline with a spent budget (%v, %v)", got, minWorkerGrace)
	}
}

func TestGraceForClipsToRemainingBudget(t *testing.T) {
	setTeardownStart(time.Now())

	{
		got := graceFor("scanner")
		require.True(t, got == workerStopGrace, "first worker grace (%v, %v)", got, workerStopGrace)
	}
	{
		got := graceFor(engineManagerWorkerName)
		require.True(t, got == engineManagerStopGrace, "engine-manager grace (%v, %v)", got, engineManagerStopGrace)
	}

	// Pretend teardown began long enough ago that the budget is gone.
	setTeardownStart(time.Now().Add(-2 * teardownBudget))
	{
		got := graceFor("scanner")
		require.True(t, got == minWorkerGrace, "grace with a spent budget (%v, %v)", got, minWorkerGrace)
	}

	// Halfway through the budget a long grace is clipped, not refused.
	setTeardownStart(time.Now().Add(-(teardownBudget - 2*time.Second)))
	got := graceFor(engineManagerWorkerName)
	require.LessOrEqual(t, got, 2*time.Second, "clipped engine-manager grace")
	require.GreaterOrEqual(t, got, time.Second, "clipped engine-manager grace")
}

func setTeardownStart(at time.Time) {
	teardownClock.mu.Lock()
	defer teardownClock.mu.Unlock()
	teardownClock.start = at
}

// TestStopGraceForGivesEngineManagerRoom guards the one worker whose shutdown does
// real work: it stops the engine processes it launched, and cutting that short is
// what orphans them.
func TestStopGraceForGivesEngineManagerRoom(t *testing.T) {
	{
		got := stopGraceFor(engineManagerWorkerName)
		require.True(t, got == engineManagerStopGrace, "engine-manager grace (%v, %v)", got, engineManagerStopGrace)
	}
	{
		got := stopGraceFor("scanner")
		require.True(t, got == workerStopGrace, "scanner grace (%v, %v)", got, workerStopGrace)
	}
	require.Greater(t, engineManagerStopGrace, workerStopGrace, "engine-manager must get more room than an ordinary worker")
	// Otherwise the longest grace is clipped the instant teardown starts and can
	// never actually be granted.
	require.Less(t, engineManagerStopGrace, teardownBudget, "longest worker grace")
}
