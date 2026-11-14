// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"time"
)

// downloadProcess is a download's vendor tool — LM Studio's `lms get`, vLLM's
// `hf` — as stopDownload sees it.
//
// No wait for one may be unbounded. Wait does not return until the process's
// stdio pipes close, a grandchild holding an inherited pipe can outlive every
// signal sent to the group, and a process the kill cannot remove at all would
// park the wait forever — and with it the cancel's JSON-RPC response, leaving
// the row on "Canceling" for good and wedging every download queued behind it
// on the engine. Giving up on the process leaks a goroutine holding a pipe,
// which is the cheaper failure.
type downloadProcess struct {
	// exited closes once the process has been reaped. Its PID is then the OS's
	// to reissue, and neither signal below addresses the process alone — each
	// reaches its group or tree — so neither is sent after it closes.
	exited <-chan struct{}
	// interrupt asks the process to stop.
	interrupt func() error
	// kill ends the process and whatever it started.
	kill func()
}

// downloadStopGraces bounds each stage of stopping a download's process.
type downloadStopGraces struct {
	// interrupt is how long the process has to exit once asked.
	interrupt time.Duration
	// retry, when set, bounds a second request to stop, for a tool whose first
	// can be lost.
	retry time.Duration
	// reap bounds the wait for a killed process. Reaching it means the kill
	// did not remove the process, which is then abandoned.
	reap time.Duration
}

// downloadStopGracesKey addresses the shortened graces a test installs on a
// context.
type downloadStopGracesKey struct{}

// downloadStopGracesFrom returns the graces installed on ctx, or else the
// tool's own.
func downloadStopGracesFrom(ctx context.Context, tool downloadStopGraces) downloadStopGraces {
	if graces, ok := ctx.Value(downloadStopGracesKey{}).(downloadStopGraces); ok {
		return graces
	}
	return tool
}

// downloadStop is how stopping a download's process went.
type downloadStop struct {
	// interruptErr is why the request to stop could not be sent, when it could
	// not. Nothing then asked the process to stop, so it is killed at once.
	interruptErr error
	// killed reports that the process had to be killed.
	killed bool
	// exited reports that the process was reaped. One still running after the
	// kill has been abandoned.
	exited bool
}

// stopDownload asks a download's process to stop, asks once more when the
// tool's graces allow a retry, and kills it if it is still running, waiting no
// longer than graces allow at each stage.
func stopDownload(p downloadProcess, graces downloadStopGraces) downloadStop {
	interrupt := func() error {
		if processExited(p.exited) {
			return os.ErrProcessDone
		}
		return p.interrupt()
	}
	var stop downloadStop
	if stop.interruptErr = interrupt(); stop.interruptErr == nil {
		stop.exited = waitForExit(p.exited, graces.interrupt)
		if !stop.exited && graces.retry > 0 && interrupt() == nil {
			stop.exited = waitForExit(p.exited, graces.retry)
		}
	}
	// A process that exited by itself — before it could be asked, or just
	// after its grace ran out — needs no kill.
	if stop.exited = stop.exited || processExited(p.exited); stop.exited {
		return stop
	}
	stop.killed = true
	p.kill()
	stop.exited = waitForExit(p.exited, graces.reap)
	return stop
}

// processExited reports whether a process has already been reaped. A
// cancellation and the process's own exit can be ready at once, and select
// picks between them at random, so a caller stopping a download checks this
// first: a run that finished stands, and its PID is no longer the caller's to
// signal.
func processExited(exited <-chan struct{}) bool {
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

// waitForExit reports whether the process was reaped within grace.
func waitForExit(exited <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
		return true
	case <-timer.C:
		return false
	}
}
