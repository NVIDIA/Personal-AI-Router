// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync"
	"time"
)

// supervisedHandle is the minimal surface the supervisor needs from a
// worker-process handle. Every broker worker handle (scannerProcess,
// proxyProcess, ...) satisfies it: a channel that closes once cmd.Wait
// returns, and a Stop that tears the process down. The supervisor never
// needs the concrete type — the per-worker spawn closure is responsible
// for wiring readers and storing the concrete handle on the Broker; the
// supervisor only watches Done and calls Stop.
type supervisedHandle interface {
	// Done returns a channel that is closed when the underlying process
	// has exited (after cmd.Wait returns). The supervisor selects on it
	// to learn that a worker died.
	Done() <-chan struct{}
	// Stop tears the worker down (closes its stdin, which the worker observes
	// as EOF and treats as its shutdown signal) and blocks until it has
	// exited. The wait is bounded and escalates to a signal and then a kill
	// (see waitForStdinClose). Safe to call multiple times.
	Stop()
}

// restartPolicy configures the hybrid auto-restart behaviour the broker
// applies to a crashed worker. The defaults (see defaultRestartPolicy)
// implement: exponential backoff between attempts, a bounded budget so a
// hard-broken binary doesn't spin forever, and a "healthy reset" window —
// a worker that stays up that long is considered recovered, which both
// clears its crash error and resets the attempt counter so a much-later
// crash gets a fresh budget rather than tripping the exhausted one.
type restartPolicy struct {
	// baseDelay is the backoff before the first restart attempt.
	baseDelay time.Duration
	// maxDelay caps the exponential backoff so it never grows unbounded.
	maxDelay time.Duration
	// maxAttempts is the restart budget within a single unhealthy streak.
	// Once exceeded the supervisor gives up and leaves the worker down
	// (its crash error stays sticky). A value <= 0 disables auto-restart
	// entirely — the crash is surfaced but the worker is not respawned.
	maxAttempts int
	// healthyReset is how long a freshly (re)started worker must stay up
	// before it's considered recovered: the attempt counter resets to 0
	// and onRecovered fires. A value <= 0 disables the healthy-reset /
	// auto-clear behaviour (every crash counts against the budget forever).
	healthyReset time.Duration
}

// defaultRestartPolicy is the hybrid policy the broker uses for every
// auto-restarted worker: ~1s -> 16s exponential backoff, a budget of 5
// attempts per unhealthy streak, and a 60s healthy-reset window. Tuned to
// recover quickly from a transient crash while not hammering a binary
// that's deterministically broken.
func defaultRestartPolicy() restartPolicy {
	return restartPolicy{
		baseDelay:    1 * time.Second,
		maxDelay:     16 * time.Second,
		maxAttempts:  5,
		healthyReset: 60 * time.Second,
	}
}

// noRestartPolicy disables auto-restart: a crashed worker is surfaced (via
// onCrash) but never respawned. It preserves the broker's historical
// "log and carry on" behaviour while still routing the worker through the
// supervision framework, and is the policy used until auto-restart is
// switched on for a worker.
func noRestartPolicy() restartPolicy {
	return restartPolicy{}
}

// backoff returns the delay before the given 1-indexed restart attempt:
// baseDelay * 2^(attempt-1), capped at maxDelay. The doubling is done in a
// loop with an early return at the cap so a large attempt count can never
// overflow the duration.
func (p restartPolicy) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := p.baseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.maxDelay {
			return p.maxDelay
		}
	}
	if d > p.maxDelay {
		return p.maxDelay
	}
	return d
}

// watchResult is the outcome of watching a single running worker handle.
type watchResult int

const (
	// watchStopped means the supervisor was asked to stop (Stop closed
	// stopCh) while the worker was still running.
	watchStopped watchResult = iota
	// watchExited means the worker process exited on its own (an
	// unexpected death the supervisor should react to).
	watchExited
	// watchRestart means Restart was called: stop the current (healthy)
	// worker and respawn it promptly, without counting it as a crash.
	watchRestart
)

// supervisor owns the lifecycle of a single broker worker: it spawns the
// worker, watches it for unexpected exit, and restarts it per the
// restartPolicy. One supervisor instance manages one logical worker
// (re-spawning produces a fresh OS process each time).
//
// The supervisor is deliberately decoupled from the concrete worker type:
// spawn returns a supervisedHandle and is expected to have already wired
// the worker's reader goroutines and stored the concrete handle on the
// Broker (under whatever lock that field needs) as a side effect. That
// keeps the broker's request handlers reading b.proxy / b.scanner / ...
// directly while the supervisor transparently swaps the process underneath
// on a restart.
type supervisor struct {
	name   string
	policy restartPolicy
	// spawn starts a fresh worker process, wires its readers, stores the
	// concrete handle on the Broker, and returns the handle for the
	// supervisor to watch. It is called once by Start and again on every
	// restart.
	spawn func() (supervisedHandle, error)

	// onCrash, if set, is invoked on every unexpected exit with the
	// 1-indexed attempt number. The broker uses it to emit a
	// supervisor:subprocess-crashed:<name> error into the nvpair-errors
	// pipeline. Invoked on the monitor goroutine.
	onCrash func(attempt int)
	// onRecovered, if set, is invoked when a restarted worker has stayed
	// up for healthyReset. The broker uses it to clear the crash error.
	// Invoked on the monitor goroutine.
	onRecovered func()
	// onExhausted, if set, is invoked once when the worker has no remaining
	// restart path and is terminally down. Invoked on the monitor goroutine
	// before it waits for Stop.
	onExhausted func(attempt int)

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}

	// restartCh requests a graceful restart of the current worker process so
	// it re-reads external state it only loads at startup — e.g. cluster
	// identity/pins that appear under --cluster-dir when this node joins a
	// cluster, flipping the worker from plain HTTP to pin-gated mTLS (a
	// bind-time decision). Buffered + coalesced via Restart.
	restartCh chan struct{}
}

// newSupervisor constructs a supervisor for one worker. onCrash and
// onRecovered are set by the caller as needed before Start.
func newSupervisor(name string, policy restartPolicy, spawn func() (supervisedHandle, error)) *supervisor {
	return &supervisor{
		name:      name,
		policy:    policy,
		spawn:     spawn,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		restartCh: make(chan struct{}, 1),
	}
}

// Start spawns the worker's first instance synchronously and, on success,
// launches the monitor goroutine that watches for exits and restarts. The
// synchronous first spawn lets a caller treat a hard spawn failure as
// fatal (e.g. the scanner) by inspecting the returned error. Stop must
// only be called after Start has returned nil.
func (s *supervisor) Start() error {
	h, err := s.spawn()
	if err != nil {
		return err
	}
	go s.monitor(h)
	return nil
}

// workerStopReportAfter is how long a worker may take to tear down before this
// starts naming it. The whole tree normally stops in well under a second, so a
// worker past this is either doing real work or wedged.
const workerStopReportAfter = 2 * time.Second

// Stop signals the supervisor to shut down and blocks until the monitor
// goroutine has torn the current worker down and exited. Idempotent.
//
// The wait is reported because it is otherwise completely silent: teardown joins
// each worker's real exit with no deadline (see waitForStdinClose), so a worker
// that never exits stops the broker here forever without logging which one it
// was. Naming it is the difference between a diagnosable report and a log that
// simply stops.
func (s *supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })

	started := time.Now()
	select {
	case <-s.doneCh:
		return
	case <-time.After(workerStopReportAfter):
	}

	slog.Warn("supervisor: worker still stopping",
		"worker", s.name, "waitedMs", time.Since(started).Milliseconds())
	<-s.doneCh
	slog.Info("supervisor: worker stopped",
		"worker", s.name, "waitedMs", time.Since(started).Milliseconds())
}

// Restart asks the supervisor to stop the current worker process and spawn a
// fresh one, without counting it as a crash. Used to re-point a worker at
// external state it only reads at startup — e.g. cluster identity/pins that
// appear under --cluster-dir when this node joins a cluster, which flip the
// worker from plain HTTP to pin-gated mTLS (a bind-time decision). Non-blocking
// and coalescing: a burst collapses to one restart. A no-op once Stopped.
func (s *supervisor) Restart() {
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
}

// monitor is the supervisor's control loop. It watches the current handle;
// on an unexpected exit it surfaces the crash, then backs off and respawns
// per the policy, until either the worker recovers (resetting the budget),
// the restart budget is exhausted (leaving the worker down with a sticky
// crash error), or Stop is called.
func (s *supervisor) monitor(h supervisedHandle) {
	defer close(s.doneCh)

	attempts := 0
	for {
		// onStable resets the attempt budget and clears the crash error
		// once the current process has stayed up for healthyReset. It runs
		// synchronously on this goroutine inside watch, so mutating
		// attempts here is race-free.
		res := s.watch(h, func() {
			if attempts != 0 {
				attempts = 0
				slog.Info("supervisor: worker recovered (stable)", "worker", s.name)
				if s.onRecovered != nil {
					s.onRecovered()
				}
			}
		})
		if res == watchStopped {
			h.Stop()
			return
		}
		if res == watchRestart {
			// Graceful, operator-driven restart (e.g. cluster certs appeared on
			// join): stop the running worker and respawn it promptly. Not a
			// crash — it surfaces no error and doesn't spend the restart budget.
			slog.Info("supervisor: restarting worker on request", "worker", s.name)
			h.Stop()
			if nh, err := s.spawn(); err == nil {
				attempts = 0
				h = nh
				continue
			} else {
				// A requested respawn failed (rare — the same command was just
				// running). Fall through to the crash/backoff path so it still
				// recovers rather than wedging the worker down.
				slog.Warn("supervisor: requested restart respawn failed; falling back to crash recovery", "worker", s.name, "err", err)
			}
		}

		// Unexpected exit (crash) — or a failed requested respawn above. Surface
		// it and decide whether to restart.
		attempts++
		slog.Warn("supervisor: worker exited unexpectedly", "worker", s.name, "attempt", attempts)
		if s.onCrash != nil {
			s.onCrash(attempts)
		}
		if s.exhausted(attempts) {
			s.notifyExhausted(attempts)
			<-s.stopCh
			return
		}

		// Back off, then respawn. A spawn failure counts as another failed
		// attempt and re-enters the backoff/budget loop rather than wedging.
		var next supervisedHandle
		for {
			if !s.sleep(s.policy.backoff(attempts)) {
				return // Stop called during backoff.
			}
			nh, err := s.spawn()
			if err == nil {
				next = nh
				break
			}
			attempts++
			slog.Warn("supervisor: respawn failed", "worker", s.name, "attempt", attempts, "err", err)
			if s.exhausted(attempts) {
				s.notifyExhausted(attempts)
				<-s.stopCh
				return
			}
		}
		slog.Info("supervisor: worker restarted", "worker", s.name, "attempt", attempts)
		h = next
	}
}

// exhausted reports whether the supervisor should stop restarting the
// worker — either because auto-restart is disabled (maxAttempts <= 0) or
// because the attempt budget for the current unhealthy streak is spent —
// and logs the reason. When it returns true the caller leaves the worker
// down (its crash error stays sticky) until Stop.
func (s *supervisor) exhausted(attempts int) bool {
	if s.policy.maxAttempts <= 0 {
		slog.Warn("supervisor: auto-restart disabled; leaving worker down", "worker", s.name)
		return true
	}
	if attempts > s.policy.maxAttempts {
		slog.Warn("supervisor: restart budget exhausted; leaving worker down", "worker", s.name, "attempts", attempts)
		return true
	}
	return false
}

func (s *supervisor) notifyExhausted(attempts int) {
	if s.onExhausted != nil {
		s.onExhausted(attempts)
	}
}

// watch blocks until the worker handle exits or the supervisor is stopped.
// If healthyReset is configured, onStable is invoked once the worker has
// been up that long (and before any exit) so the caller can mark it
// recovered. Returns watchStopped if Stop was called while the worker was
// still running, or watchExited if the process exited on its own.
func (s *supervisor) watch(h supervisedHandle, onStable func()) watchResult {
	var healthyC <-chan time.Time
	if s.policy.healthyReset > 0 {
		t := time.NewTimer(s.policy.healthyReset)
		defer t.Stop()
		healthyC = t.C
	}
	for {
		select {
		case <-s.stopCh:
			return watchStopped
		case <-s.restartCh:
			return watchRestart
		case <-healthyC:
			// Fire once: nil-out the channel so this arm blocks forever
			// after the first tick (the worker is now considered stable).
			healthyC = nil
			if onStable != nil {
				onStable()
			}
		case <-h.Done():
			return watchExited
		}
	}
}

// sleep waits for d, returning true if the full delay elapsed and false if
// Stop was called first. A non-positive delay returns immediately (true
// unless already stopped) so a zero backoff still honours a pending stop.
func (s *supervisor) sleep(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-s.stopCh:
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-t.C:
		return true
	}
}
