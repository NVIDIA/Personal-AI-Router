// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// These pin the shutdown waits that let the mDNS responder's goodbye
// retraction (RFC 6762 §10.1) leave the machine before the process exits:
// Scanner.Run joins the daemon, and daemon.run joins responder.Run. The
// Scanner.Run half is exercised end-to-end by
// TestScannerRun_DoesNotReturnBeforeTheDaemonHasShutDownWhenStdinEOFs; the
// per-interface send it is waiting for is pinned in the shared mdns
// package's goodbye tests, and live SIGTERM wire capture is a Task 7
// deliverable.

func TestDaemonBrowseLoop_WaitsForResponderAfterEventsClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := make(chan DiscoveryEvent)
		close(events)
		responderDone := make(chan struct{})
		returned := make(chan struct{})
		d := &daemon{}
		go func() {
			// Keep the context live to force the closed-events exit path.
			d.runBrowseLoop(context.Background(), events, responderDone)
			close(returned)
		}()

		synctest.Wait()
		select {
		case <-returned:
			t.Error("daemon returned before the responder finished its goodbye")
		default:
		}

		close(responderDone)
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Error("daemon did not return after the responder finished")
		}
	})
}

func TestWaitForShutdown_ReturnsImmediatelyWhenNothingToJoin(t *testing.T) {
	// A daemon built without an mDNS responder hands down a nil done
	// channel: there is no retraction waiting to be sent, so shutdown must
	// not pay the join timeout for it.
	logs := captureLogs(t)
	start := time.Now()
	waitForShutdown(nil, 5*time.Second, "_nvpair-node mDNS responder")
	assert.Less(t, time.Since(start), time.Second,
		"a nil done channel means nothing to wait for, so the wait must not block")
	assert.NotContains(t, logs.String(), "gave up",
		"joining nothing is not a failed shutdown and must not log a give-up warning")
}

func TestWaitForShutdown_ReturnsAsSoonAsDoneCloses(t *testing.T) {
	logs := captureLogs(t)
	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()
	start := time.Now()
	waitForShutdown(done, 5*time.Second, "_nvpair-node mDNS responder")
	assert.Less(t, time.Since(start), time.Second,
		"the wait must end on done closing, not on the timeout expiring")
	assert.NotContains(t, logs.String(), "gave up",
		"a shutdown that finished inside its budget must not be reported as timed out")
}

func TestWaitForShutdown_WarnsNamingTheComponentWhenTheWaitExpires(t *testing.T) {
	// Giving up means peers keep our dead node's records until the record
	// TTL expires, so the wait that expired has to be visible at the warn
	// level the desktop configures by default, and name the component that
	// never came back — without the name, a give-up line says only that
	// some anonymous join lost, not which retraction was lost.
	logs := captureLogs(t)
	start := time.Now()
	waitForShutdown(make(chan struct{}), 10*time.Millisecond, "_nvpair-node mDNS responder")
	assert.GreaterOrEqual(t, time.Since(start), 10*time.Millisecond,
		"the wait must honor the timeout it was given, not return early")

	gotLogs := logs.String()
	assert.Contains(t, gotLogs, "level=WARN",
		"a shutdown wait that timed out is retraction loss and must not hide below the desktop's default log level")
	assert.Contains(t, gotLogs, `component="_nvpair-node mDNS responder"`,
		"the give-up line must name which component never returned, i.e. whose retraction was lost")
}

// TestScannerRun_DoesNotReturnBeforeTheDaemonHasShutDownWhenStdinEOFs pins
// the deadlock trap and the join together on the real code path: broker
// gone means stdin EOFs while ctx is still live, so Scanner.Run must cancel
// the daemon's context itself before joining it. Forget that cancel and the
// daemon sits in its ctx.Done select until the join times out — Run still
// returns, but two seconds late and with a give-up warning, which is what
// this test fails on. (A permanent hang cannot survive the test binary's
// own timeout, and the 5s wait here is far below go test's default 10m.)
func TestScannerRun_DoesNotReturnBeforeTheDaemonHasShutDownWhenStdinEOFs(t *testing.T) {
	logs := captureLogs(t)
	clusterDir := t.TempDir()
	sc := NewScanner(
		NewCodec(&stdioTransport{Reader: strings.NewReader(""), Writer: io.Discard}),
		clustertrust.Open(clusterDir), clusterDir)

	returned := make(chan error, 1)
	go func() { returned <- sc.Run(context.Background()) }()

	select {
	case err := <-returned:
		require.NoError(t, err, "stdin EOF is a clean broker departure, not an error to report to the broker")
	case <-time.After(5 * time.Second):
		t.Fatal("Scanner.Run never returned after stdin EOF: shutdown deadlocked")
	}

	assert.NotContains(t, logs.String(), "gave up",
		"Scanner.Run reached its daemon join only through its own cancel; if the daemon had to wait out the join timeout the explicit cancel before the wait is missing")
}
