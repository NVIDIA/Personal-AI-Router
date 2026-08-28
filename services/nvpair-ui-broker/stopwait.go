// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	// workerStopGrace is how long a worker gets to exit on its own after its
	// stdin closes. The whole tree normally stops in a fraction of a second, so
	// this is already generous: it exists so teardown always ends, not to race a
	// worker that is doing real work.
	workerStopGrace = 3 * time.Second

	// engineManagerStopGrace is longer because nvpair-engine-manager stops the
	// engine processes it launched (StopAll) on its way out. Cutting that short is
	// exactly what the old 2s-grace-then-kill did, and it orphaned engines.
	// graceFor reserves this out of the shared budget so the workers joined ahead
	// of it cannot spend it — it is joined eighth of eleven, so join order alone
	// would otherwise decide how much it gets.
	engineManagerStopGrace = 6 * time.Second

	// engineStopAllBudget bounds the broker's wait for engine-manager to stop the
	// engines (engine:prepare-shutdown). This step ran with no deadline at all,
	// which meant the one genuinely open-ended part of shutdown could consume the
	// parent's whole window before a single worker had been joined.
	engineStopAllBudget = 5 * time.Second

	// workerSignalGrace is how long a signalled worker gets before it is killed,
	// and how long a killed worker gets before the join gives up on it.
	workerSignalGrace = 2 * time.Second

	// teardownBudget bounds everything the broker does between being told to shut
	// down and its last worker join. The joins run one after another, so
	// per-worker graces alone add up: a tree where each of the eleven workers
	// hangs for its own grace outlives the parent's shutdown grace several times
	// over, and being killed from outside mid-teardown skips the rest of shutdown.
	//
	// The accounting the parent's grace has to cover, in order:
	//
	//	beginTeardown … last worker join   <= teardownBudget          (10s)
	//	workload history shutdown flush    <= workloadHistoryFlushJoinTimeout (5s)
	//
	// which is the 15s the desktop parent and nvpair-tui both allow. Everything
	// inside the first line — the two proxy joins, the engine StopAll wait, and
	// all eleven joins — draws on this one budget, so no step can be lengthened
	// without the others giving way. It must also exceed the largest single worker
	// grace, or that worker could never be granted it.
	teardownBudget = 10 * time.Second

	// minWorkerGrace is the floor every worker keeps even once the budget is
	// spent, so a worker milliseconds away from exiting is not signalled purely
	// because earlier workers were slow.
	minWorkerGrace = 250 * time.Millisecond

	// engineManagerWorkerName selects the longer grace above.
	engineManagerWorkerName = "engine-manager"
)

// teardownClock records when shutdown began.
//
// It is armed explicitly rather than by the first join, because a join also
// happens at ordinary runtime: supervisor.Restart stops the current worker before
// spawning a replacement, which LM Studio port reconciliation does on a rebind.
// Arming the clock there would leave the budget permanently spent for the life of
// the process, so every later join — including engine-manager's at the real
// shutdown — would fall to minWorkerGrace and be signalled almost immediately.
var teardownClock struct {
	mu    sync.Mutex
	start time.Time
}

// beginTeardown arms the shared budget. Called once, from the shutdown path.
func beginTeardown() {
	teardownClock.mu.Lock()
	defer teardownClock.mu.Unlock()
	if teardownClock.start.IsZero() {
		teardownClock.start = time.Now()
	}
}

// teardownRemaining reports what is left of the budget, and whether the budget
// applies at all. A Stop outside teardown (supervisor.Restart) is not clipped:
// there is no sequence of joins for it to add up with, and a restarting worker
// still deserves its full grace to drain.
func teardownRemaining() (time.Duration, bool) {
	teardownClock.mu.Lock()
	defer teardownClock.mu.Unlock()
	if teardownClock.start.IsZero() {
		return 0, false
	}
	return teardownBudget - time.Since(teardownClock.start), true
}

func resetTeardownClock() {
	teardownClock.mu.Lock()
	defer teardownClock.mu.Unlock()
	teardownClock.start = time.Time{}
}

// stopGraceFor returns how long the named worker may take to exit on its own,
// ignoring the shared budget.
func stopGraceFor(name string) time.Duration {
	if name == engineManagerWorkerName {
		return engineManagerStopGrace
	}
	return workerStopGrace
}

// graceFor returns the named worker's own grace, clipped to what is left of the
// shared teardown budget and floored at minWorkerGrace. One slow worker still
// gets its full grace, while many cannot add up past the budget.
func graceFor(name string) time.Duration {
	grace := stopGraceFor(name)
	left, active := teardownRemaining()
	if !active {
		return grace
	}
	if name != engineManagerWorkerName {
		left -= engineManagerStopGrace
	}
	if left < grace {
		grace = left
	}
	if grace < minWorkerGrace {
		grace = minWorkerGrace
	}
	return grace
}

// engineShutdownDeadline bounds the wait for engine-manager to stop its engines,
// drawing on the same budget as the joins that follow it.
func engineShutdownDeadline() time.Duration {
	d := engineStopAllBudget
	if left, active := teardownRemaining(); active && left < d {
		d = left
	}
	if d < minWorkerGrace {
		d = minWorkerGrace
	}
	return d
}

// waitForStdinClose signals a managed worker to shut down by closing its stdin
// (which every worker observes as EOF and treats as its shutdown signal), then
// waits for the process to actually exit (done is closed by the cmd.Wait
// goroutine). The wait is bounded and escalates: grace, terminate, kill, give up.
// The grace is also clipped to a teardown-wide budget (see graceFor), so the
// sequential joins across the whole tree cannot add up past it.
//
// This join was previously unbounded, on the reasoning that each worker owns its
// own bounded shutdown, so a worker that fails to exit on EOF is a worker bug to
// fix rather than something the broker should paper over by force-killing. That
// reasoning holds only while nothing outside the worker can prevent its exit —
// and something could. Every worker inherited the broker's stderr, so a parent
// that stopped reading that pipe left a worker blocked inside write() and unable
// to reach exit through no fault of its own, and this join then deadlocked the
// entire teardown until the broker was force-killed from outside, skipping its
// remaining shutdown work. stderrsink.go removes that specific cause; the bound
// here is what keeps the next equivalent from being an unrecoverable hang.
//
// Terminate precedes kill because a worker parked in a blocking write can still
// run its signal handler and exit cleanly, whereas a kill loses whatever it was
// part-way through. Giving up after the kill is deliberate: a process that
// survives SIGKILL will not be reaped by waiting longer, and the broker's own
// remaining teardown is worth more than that one join.
//
// Safe to call multiple times: closing an already-closed stdin is ignored and
// receiving on an already-closed done channel returns immediately.
func waitForStdinClose(name string, cmd *exec.Cmd, stdin io.Closer, done <-chan struct{}) {
	_ = stdin.Close()

	started := time.Now()
	grace := graceFor(name)
	if waitClosed(done, grace) {
		return
	}

	proc := startedProcess(cmd)
	if proc == nil {
		slog.Warn("worker has not exited and exposes no process handle; abandoning the join",
			"worker", name, "waitedMs", time.Since(started).Milliseconds())
		return
	}

	if terminateIsKill && name == engineManagerWorkerName {
		// Here the terminate stage is a TerminateProcess, so signalling would
		// interrupt StopAll and orphan the engines this worker launched — and the
		// process group it runs in is not a Job object, so the OS will not reap
		// them either. An abandoned join costs the rest of this worker's teardown;
		// a kill costs the user a stray engine process.
		slog.Error("engine-manager did not exit and cannot be signalled without killing it mid-teardown; abandoning the join",
			"worker", name, "waitedMs", time.Since(started).Milliseconds())
		return
	}

	slog.Warn("worker did not exit when its stdin closed; sending terminate",
		"worker", name, "graceMs", grace.Milliseconds())
	if err := signalTerminate(proc); err != nil {
		slog.Warn("terminate signal failed", "worker", name, "err", err)
	}
	if waitClosed(done, workerSignalGrace) {
		slog.Info("worker exited after terminate",
			"worker", name, "waitedMs", time.Since(started).Milliseconds())
		return
	}

	slog.Error("worker ignored terminate; killing", "worker", name)
	if err := proc.Kill(); err != nil {
		slog.Warn("kill failed", "worker", name, "err", err)
	}
	if !waitClosed(done, workerSignalGrace) {
		slog.Error("worker did not exit after kill; abandoning the join",
			"worker", name, "waitedMs", time.Since(started).Milliseconds())
		return
	}
	slog.Warn("worker killed", "worker", name, "waitedMs", time.Since(started).Milliseconds())
}

// waitClosed reports whether done closed within d.
func waitClosed(done <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// startedProcess returns the OS process for a worker that was actually started,
// or nil when there is nothing to signal.
func startedProcess(cmd *exec.Cmd) *os.Process {
	if cmd == nil {
		return nil
	}
	return cmd.Process
}
