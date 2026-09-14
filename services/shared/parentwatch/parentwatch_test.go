// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package parentwatch

import (
	"os"
	"testing"
	"time"
)

// The whole point: a parent that goes away must take its children with it.
// os.Getppid() will not equal a pid we invent, so this is exactly the
// "my parent changed" condition an orphan observes.
func TestFiresWhenTheParentIsGone(t *testing.T) {
	fired := make(chan struct{})
	stop := start("test", os.Getppid()+100000, time.Millisecond, time.Hour, func() { close(fired) }, func(int) {})
	defer stop()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("parent went away and nothing fired; the process would be orphaned")
	}
}

func TestQuietWhileTheParentIsAlive(t *testing.T) {
	fired := make(chan struct{})
	stop := start("test", os.Getppid(), time.Millisecond, time.Hour, func() { close(fired) }, func(int) {})
	defer stop()

	select {
	case <-fired:
		t.Fatal("shut down while the parent was still alive")
	case <-time.After(150 * time.Millisecond):
	}
}

// A service launched by something that is already init -- nohup, a launchd job,
// a detached harness -- has no parent to outlive. Testing for pid 1 instead of
// comparing against the pid we started under would make those exit instantly.
func TestDoesNotWatchWhenThereIsNoParentToOutlive(t *testing.T) {
	for _, ppid := range []int{1, 0, -1} {
		fired := make(chan struct{})
		stop := start("test", ppid, time.Millisecond, time.Hour, func() { close(fired) }, func(int) {})
		select {
		case <-fired:
			t.Errorf("started with ppid %d and shut itself down; it has no parent to lose", ppid)
		case <-time.After(50 * time.Millisecond):
		}
		stop()
	}
}

func TestStopIsIdempotentAndHaltsTheWatch(t *testing.T) {
	fired := make(chan struct{}, 1)
	stop := start("test", os.Getppid()+100000, 20*time.Millisecond, time.Hour, func() { fired <- struct{}{} }, func(int) {})
	stop()
	stop() // must not panic on a double close

	select {
	case <-fired:
		t.Fatal("fired after stop")
	case <-time.After(100 * time.Millisecond):
	}
}

// A shutdown that never finishes is how the original orphan survived: it
// ignored SIGTERM too. The watchdog must end the process itself rather than
// wait forever on a graceful path that is not coming.
func TestExitsWhenGracefulShutdownWedges(t *testing.T) {
	exited := make(chan int, 1)
	stop := start("test", os.Getppid()+100000, time.Millisecond, 20*time.Millisecond,
		func() { /* wedged: never completes */ },
		func(code int) { exited <- code })
	defer stop()

	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("exit code %d; being orphaned is not a crash", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown wedged and the process was left running -- the orphan this prevents")
	}
}
