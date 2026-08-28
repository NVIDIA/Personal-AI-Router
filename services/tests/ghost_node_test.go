// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// TestScannerEvictsRecordSupersededAtItsAddress is the cross-process regression
// test for the duplicate ("ghost") node a reset machine leaves behind.
//
// A machine whose appdata is wiped mints a fresh hostUuid but keeps its LAN
// address and re-binds the same fixed service ports. The scanner's
// evict-on-mDNS-miss guard probes a missing node before dropping it, and a
// TCP-only probe reached the machine's NEW incarnation on those same ports and
// reported the OLD record alive — forever. The probe now confirms identity, so
// this exercises the whole chain in one process tree: mDNS browse -> miss
// threshold -> liveness probe -> node-info identity mismatch -> node-removed.
//
// The peer is simulated rather than a second PAIR install: a zeroconf
// responder publishes the record, and a local HTTP server plays its node-info.
// Flipping that server's reported hostUuid is exactly the signal a wipe
// produces, and is what the probe must notice.
func TestScannerEvictsRecordSupersededAtItsAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("multicast + miss-threshold timing; skipped under -short")
	}

	const (
		peerInstance = "ghost-peer-host"
		originalUUID = "11111111-aaaa-4aaa-8aaa-000000000001"
		wipedUUID    = "22222222-bbbb-4bbb-8bbb-000000000002"
	)

	// The peer's node-info. It reports originalUUID until the "wipe", after
	// which it reports wipedUUID from the same address and port — the machine is
	// up and listening throughout, which is precisely why a TCP probe cannot
	// tell the two apart.
	reported := &atomic.Value{}
	reported.Store(originalUUID)
	nodeInfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"GPUs":[],"hostUuid":"` + reported.Load().(string) + `"}`))
	}))
	defer nodeInfo.Close()

	_, niPortStr, err := net.SplitHostPort(strings.TrimPrefix(nodeInfo.URL, "http://"))
	if err != nil {
		t.Fatalf("node-info addr: %v", err)
	}
	niPort, err := strconv.Atoi(niPortStr)
	if err != nil {
		t.Fatalf("node-info port: %v", err)
	}

	// Publish the peer's _nvpair-node record pointing at that node-info.
	txt := []string{
		"v=" + noderec.SchemaVersion,
		"uuid=" + originalUUID,
		"ip=127.0.0.1",
		fmt.Sprintf("%s=%d", noderec.ServiceNodeInfo, niPort),
	}
	responder, err := zeroconf.Register(peerInstance, noderec.ServiceType, testDomain, noderec.SRVPort, txt, nil)
	if err != nil {
		t.Fatalf("register peer: %v", err)
	}
	responderDown := false
	defer func() {
		if !responderDown {
			responder.Shutdown()
		}
	}()

	events := startScannerForGhostTest(t)

	// The scanner must see the peer before there is anything to evict. A slow
	// or lossy multicast environment is an inconclusive run, not a failure.
	if !awaitNodeEvent(events, noderec.NotifyNodeDiscovered, originalUUID, 30*time.Second) {
		t.Skip("peer record was never discovered; multicast is unavailable in this environment")
	}

	// The wipe: the record stops being advertised under the old identity, and
	// the machine now answers as someone else at the same address and port.
	responder.Shutdown()
	responderDown = true
	reported.Store(wipedUUID)

	// Eviction waits on the browser's full miss threshold (12 scans at 5s, a full
	// minute) — the window is sized to outlast a node saturated by its own
	// inference load — so this deadline is generous by design. The identity probe
	// runs on every scan from ~15s in, so the mismatch is detected long before the
	// eviction it eventually authorizes.
	if !awaitNodeEvent(events, noderec.NotifyNodeRemoved, originalUUID, 2*time.Minute) {
		t.Fatalf("scanner never evicted %s after its address began answering as %s; "+
			"a record whose machine was re-identified must not be kept alive by a TCP probe",
			originalUUID, wipedUUID)
	}
}

// startScannerForGhostTest runs the scanner binary on stdio and returns its
// notification stream. The scanner emits discovery:node-* unconditionally, so
// no subscription is needed. stdin is held open for the process lifetime —
// closing it ends the scanner's read loop.
//
// --cluster-dir is a throwaway temp dir, as everywhere else in this package.
// Without it the scanner resolves the real per-user app dir: it would read (and
// mint into) the developer's own identity files and advertise their machine's
// real cluster identity on the LAN while the test runs.
func startScannerForGhostTest(t *testing.T) <-chan jsonrpc.Message {
	t.Helper()
	cmd := exec.Command(scannerBin, "--cluster-dir", t.TempDir())
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("scanner stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("scanner stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scanner: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return startMsgReader(stdout)
}

// awaitNodeEvent reports whether the named discovery:node-* event for hostUUID
// arrives before the deadline. Other events (this node's own record, the peer's
// enrichment updates) are ignored.
func awaitNodeEvent(events <-chan jsonrpc.Message, method, hostUUID string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-events:
			if !ok {
				return false
			}
			if msg.Method != method {
				continue
			}
			var ev noderec.NodeEvent
			if err := json.Unmarshal(msg.Params, &ev); err != nil {
				continue
			}
			if ev.Node.HostUUID == hostUUID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
