// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

// Package parentwatch ends a subprocess when the process that started it goes
// away, so a crashed or force-quit parent cannot leave a service running.
//
// Why this exists rather than relying on stdin closing:
//
// Every service here already reads JSON-RPC from stdin and treats EOF as "my
// parent is gone". That is the right signal and it usually works, but it is not
// sufficient, because EOF only arrives when the LAST holder of the pipe's write
// end closes it. An Electron app spawns helper processes that inherit the open
// descriptors of the main process; when the main process dies but a helper
// lingers, the pipe stays open, no EOF is ever delivered, and the whole service
// tree keeps running.
//
// That is not hypothetical. It stranded a full 12-process tree for over an hour:
// the app was quit, its helpers outlived it, the broker never saw EOF, and the
// orphans went on holding ports 14318-14323. The next launch could not bind
// them, so four workers crash-looped until the supervisor gave up, and the UI
// showed an empty node list because the cluster-manager it needed was one of
// them. Nothing in the logs said "orphan"; it looked like a cluster fault.
//
// Watching the parent directly has none of that fragility. It does not care how
// many descriptors are open or who inherited them: on Unix an orphan is
// reparented (to init/launchd), so a changed parent pid IS the death of the
// original parent, observed from the child.
package parentwatch

import (
	"log/slog"
	"os"
	"time"
)

// pollInterval is how often the parent is checked.
//
// One second: an orphan holding a port blocks the next launch, so minutes of
// lag would be user-visible, while the check itself is a getppid() call --
// cheaper than the logging one line of output costs.
const pollInterval = time.Second

// exitGrace is how long a graceful shutdown gets before the process is ended
// outright.
//
// Graceful-only is not enough here. The broker that stranded the tree ignored
// SIGTERM as well as EOF -- its shutdown path was waiting on a parent that no
// longer existed -- so a watchdog that merely asks nicely can leave exactly the
// orphan it was added to prevent. Five seconds is longer than any clean
// shutdown on this path takes and far shorter than a user waits before
// launching again.
const exitGrace = 5 * time.Second

// Start watches the calling process's parent and runs onOrphaned once, in its
// own goroutine, when that parent goes away. It returns a stop function.
//
// The original parent pid is captured at call time and compared, rather than
// testing for pid 1. A service started by a launcher that is ALREADY init --
// nohup, a launchd job, a detached test harness -- would look orphaned from its
// first instant under a pid-1 test and exit immediately. Comparing against the
// pid we actually started under makes "my parent changed" mean what it says.
//
// A process whose parent is init at startup is therefore never watched: it has
// no parent to outlive, and there is nothing to detect.
func Start(name string, shutdown func()) (stop func()) {
	return start(name, os.Getppid(), pollInterval, exitGrace, shutdown, os.Exit)
}

// start is Start with the parent pid and interval injected, so a test can drive
// it without spawning real processes.
func start(
	name string,
	originalPPID int,
	interval, grace time.Duration,
	shutdown func(),
	exit func(int),
) func() {
	done := make(chan struct{})
	if originalPPID <= 1 {
		// Already parentless (or unknowable). Nothing to watch for.
		slog.Debug("parent watch not started; no parent to outlive", "service", name)
		return func() {}
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if os.Getppid() == originalPPID {
					continue
				}
				slog.Warn("parent process is gone; shutting down to avoid orphaning",
					"service", name, "originalParent", originalPPID, "now", os.Getppid())
				shutdown()
				// Backstop: a shutdown that wedges must not become the orphan
				// this exists to prevent. Exit 0 -- being orphaned is not a
				// failure of this process, and a non-zero code would read as a
				// crash to whatever restarts it.
				select {
				case <-done:
				case <-time.After(grace):
					slog.Warn("graceful shutdown did not finish; exiting",
						"service", name, "grace", grace)
					exit(0)
				}
				return
			}
		}
	}()

	var stopped bool
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
	}
}
