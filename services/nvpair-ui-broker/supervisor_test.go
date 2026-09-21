// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"testing"
	"time"
)

// fakeHandle is a test double for supervisedHandle: Done() exposes a
// channel the test closes to simulate an unexpected exit, and Stop()
// records that the supervisor tore it down.
type fakeHandle struct {
	done     chan struct{}
	stopOnce sync.Once
	stoppedC chan struct{}
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{done: make(chan struct{}), stoppedC: make(chan struct{})}
}

func (h *fakeHandle) Done() <-chan struct{} { return h.done }

func (h *fakeHandle) Stop() { h.stopOnce.Do(func() { close(h.stoppedC) }) }

// crash closes the handle's done channel, simulating an unexpected exit.
func (h *fakeHandle) crash() { close(h.done) }

// A fresh worker process has none of the state the broker pushed into its
// predecessor, so onSpawned has to fire for the first spawn and every respawn.
// Missing the respawn is the interesting failure: the worker comes back and
// silently serves without the ranking the broker believes it has.
func TestOnSpawnedFiresForFirstSpawnAndEveryRespawn(t *testing.T) {
	handles := make(chan *fakeHandle, 4)
	spawned := make(chan struct{}, 4)

	sup := newSupervisor("replay-probe", defaultRestartPolicy(), func() (supervisedHandle, error) {
		h := newFakeHandle()
		handles <- h
		return h, nil
	})
	sup.onSpawned = func() { spawned <- struct{}{} }
	sup.policy.baseDelay = time.Millisecond
	sup.policy.maxDelay = time.Millisecond

	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(sup.Stop)

	first := <-handles
	waitSupervisorSignal(t, spawned, "onSpawned did not fire for the first spawn")

	first.crash()
	<-handles
	waitSupervisorSignal(t, spawned, "onSpawned did not fire for the respawn")
}

// onSpawned must not run on the monitor goroutine: that goroutine is the only
// thing that reaches h.Stop() when stopCh closes, so a callback blocking there
// makes Stop wait it out inside the shared teardown budget.
func TestOnSpawnedDoesNotBlockStop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	entered := make(chan struct{})

	sup := newSupervisor("blocking-replay", noRestartPolicy(), func() (supervisedHandle, error) {
		return newFakeHandle(), nil
	})
	var once sync.Once
	sup.onSpawned = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitSupervisorSignal(t, entered, "onSpawned never ran")

	stopped := make(chan struct{})
	go func() {
		sup.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked behind a long-running onSpawned callback")
	}
}

func waitSupervisorSignal(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func TestRestartPolicyBackoff(t *testing.T) {
	p := restartPolicy{baseDelay: time.Second, maxDelay: 16 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 16 * time.Second}, // capped
		{100, 16 * time.Second},
	}
	for _, c := range cases {
		if got := p.backoff(c.attempt); got != c.want {
			t.Errorf("backoff(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

// fastPolicy is a unit-test policy: near-instant backoff so restarts don't
// slow the test, with a configurable budget and healthy-reset window.
func fastPolicy(maxAttempts int, healthyReset time.Duration) restartPolicy {
	return restartPolicy{
		baseDelay:    time.Millisecond,
		maxDelay:     5 * time.Millisecond,
		maxAttempts:  maxAttempts,
		healthyReset: healthyReset,
	}
}

func TestSupervisorSurfacesAndRestartsThenRecovers(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)
	recovered := make(chan struct{}, 8)

	// healthyReset short enough to observe recovery quickly.
	sup := newSupervisor("test", fastPolicy(5, 50*time.Millisecond), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	sup.onRecovered = func() { recovered <- struct{}{} }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)
	h0.crash()

	if got := recvInt(t, crashes); got != 1 {
		t.Fatalf("first onCrash attempt = %d, want 1", got)
	}

	// A fresh handle must be spawned (the restart).
	h1 := mustSpawn(t, spawned)

	// h1 stays up past healthyReset → onRecovered fires and the crash
	// error is cleared.
	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onRecovered after a stable restart")
	}

	// Keep h1 referenced so it isn't flagged unused.
	_ = h1
}

func TestSupervisorGivesUpAfterBudget(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	// Budget of 2 restarts; long healthyReset so attempts never reset.
	sup := newSupervisor("test", fastPolicy(2, time.Hour), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	// Initial + 2 restarts = 3 handles; the 3rd crash exhausts the budget
	// (attempt 3 > maxAttempts 2), so no 4th handle is spawned.
	for want := 1; want <= 3; want++ {
		h := mustSpawn(t, spawned)
		h.crash()
		if got := recvInt(t, crashes); got != want {
			t.Fatalf("onCrash attempt = %d, want %d", got, want)
		}
	}

	select {
	case <-spawned:
		t.Fatal("supervisor spawned a 4th worker after exhausting its restart budget")
	case <-time.After(200 * time.Millisecond):
		// No further spawn — correct.
	}
}

func TestSupervisorNoRestartPolicy(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	sup := newSupervisor("test", noRestartPolicy(), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)
	h0.crash()

	if got := recvInt(t, crashes); got != 1 {
		t.Fatalf("onCrash attempt = %d, want 1", got)
	}
	select {
	case <-spawned:
		t.Fatal("noRestartPolicy spawned a replacement worker")
	case <-time.After(200 * time.Millisecond):
		// No restart — correct.
	}
}

func TestSupervisorStopTearsDownCurrentHandle(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	sup := newSupervisor("test", fastPolicy(5, time.Hour), spawn)
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h0 := mustSpawn(t, spawned)

	done := make(chan struct{})
	go func() { sup.Stop(); close(done) }()

	select {
	case <-h0.stoppedC:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not tear down the running handle")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// A healthy worker is never torn down and respawned on its own.
//
// This replaces a test of the graceful-restart mechanism, which had no
// production caller and is now deleted: its purpose — re-pointing a worker at
// cluster certs it only read at startup — went away when the proxy gained a
// live trust watch, and the last caller went with the LM Studio port restarts
// that became facade-scoped. What remains worth asserting is the invariant
// that replaced it: nothing short of an actual exit replaces a running worker,
// so a facade-level failure cannot cost every engine its listener.
func TestSupervisorLeavesAHealthyWorkerAlone(t *testing.T) {
	spawned := make(chan *fakeHandle, 8)
	spawn := func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	}
	crashes := make(chan int, 8)

	sup := newSupervisor("test", fastPolicy(5, time.Hour), spawn)
	sup.onCrash = func(attempt int) { crashes <- attempt }
	if err := sup.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()

	h0 := mustSpawn(t, spawned)

	select {
	case <-h0.stoppedC:
		t.Fatal("a healthy worker was stopped without exiting")
	case h := <-spawned:
		t.Fatalf("a second worker was spawned alongside a healthy one: %v", h)
	case a := <-crashes:
		t.Fatalf("a healthy worker surfaced a crash (attempt %d)", a)
	case <-time.After(300 * time.Millisecond):
	}

	// A real exit still recovers, so the above is not simply a dead supervisor.
	h0.crash()
	_ = mustSpawn(t, spawned)
	select {
	case <-crashes:
	case <-time.After(2 * time.Second):
		t.Fatal("an actual exit did not surface a crash")
	}
}

func mustSpawn(t *testing.T, spawned <-chan *fakeHandle) *fakeHandle {
	t.Helper()
	select {
	case h := <-spawned:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a (re)spawn")
	}
	return nil
}

func recvInt(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting on channel")
	}
	return 0
}
