// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

// Cross-process proof of remote Custom-endpoint editing (Task 11).
//
// The engine-manager and broker unit tests pin each carrier half in isolation;
// this file proves the whole chain with only real binaries and real crypto:
// node A's engine-manager (driven over its stdio, the shape the broker relay
// would send) dials node B's ec mTLS surface, B's engine-manager forwards the
// POST down its stdio as a custom:relay-request, and B's broker executes the
// mutation through the same custom plane a local edit uses — so the assertion
// is the one that matters: B's custom-endpoints.json actually changed.
//
// The API-key policy is asserted, not just commented: the add body is the
// key's only sanctioned network hop, so the marker key must appear in the
// persisted store (at rest, 0600) and nowhere on an output stream — not in
// A's JSON-RPC responses, not in a single stderr line of B's stack.
//
// Like remote_engine_test.go this bypasses mDNS: A's peer directory is seeded
// with a raw discovery:nodes frame. The ec surface lives on the fixed control
// port (14323), which the test cannot relocate, so a host already holding it
// skips rather than fails.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// customE2EKey is a marker, not a credential. It must appear exactly in the
// persisted store and never on a wire or log surface.
const customE2EKey = "sk-e2e-marker-not-a-real-key-0123456789"

// customE2ENet is the unroutable TEST-NET-1 address standing in for a real
// external endpoint: validation and persistence never dial it, so no test can
// send a key (or anything else) toward a host anyone owns.
const customE2ENet = "http://203.0.113.7"

// customE2EAssertNoKey scans everything the test can still see of the
// output sides (stderr lines drained within the window) for the key.
func customE2EAssertNoKey(t *testing.T, lines <-chan string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			require.NotContains(t, line, customE2EKey,
				"the endpoint credential appeared on a log stream")
		case <-deadline:
			return
		}
	}
}

// customE2EStorePath walks the config dir for custom-endpoints.json — the
// per-user data layout differs by platform (macOS Library/Application Support
// vs XDG), and the test must find the file wherever appdir puts it.
func customE2EStorePath(dir string) string {
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "custom-endpoints.json" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// customE2EStartTarget brings up node B's full stack: the real broker
// supervising the real engine-manager, which serves the custom routes on the
// fixed ec port. Returns B's isolated config dir (where the broker's
// custom-endpoints.json must appear if anything writes it) and B's stderr
// stream for the key-scan assertions.
func customE2EStartTarget(t *testing.T, clusterDir string) (string, <-chan string) {
	t.Helper()
	cfgDir := t.TempDir()
	_, bMsgs, bStderr, bCleanup := startBrokerWithDirs(t, cfgDir, clusterDir,
		"--engine-manager-path", engineMgrBin)
	t.Cleanup(bCleanup)
	waitForMethod(t, bMsgs, "app:ready", 30*time.Second)
	waitForPort(t, "127.0.0.1", 14323, 30*time.Second)
	return cfgDir, bStderr
}

// customE2ESeedPeer points A's engine-manager at B: one discovery:nodes frame
// carrying the ec service and B's cluster uuid — the two inputs the pin gate
// and dial need, shaped exactly as the broker's relay would deliver them.
func customE2ESeedPeer(t *testing.T, aStdin io.WriteCloser, clusterUUID string) {
	t.Helper()
	snapshot := fmt.Sprintf(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{"nodes":[`+
		`{"hostUuid":"nodeB","name":"nodeB","ip":"127.0.0.1","clusterUuid":%q,"trusted":true,`+
		`"services":{"ec":{"port":14323}},"lastSeen":0}]}}`, clusterUUID)
	writeRawFrame(t, aStdin, snapshot)
}

// TestCustomRemoteEdit_TrustedPeerEdit_LandsInOwnerStoreAndKeepsKeyOffStreams drives the
// complete carrier for a paired (cross-pinned) pair: a remote add persists on
// B with the key at rest, list reads it back key-blind, and remove retracts
// it — while the marker key appears on no response body or log line.
func TestCustomRemoteEdit_TrustedPeerEdit_LandsInOwnerStoreAndKeepsKeyOffStreams(t *testing.T) {
	if portBusy(14322) || portBusy(14323) {
		t.Skip("engine-manager fixed ports 14322/14323 already in use; skipping")
	}

	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "custom-a", "custom-b"
	certA := mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB)
	writePin(t, dirB, uuidA, certA)

	cfgB, bStderr := customE2EStartTarget(t, dirB)

	aStdin, aMsgs, aCleanup := startEngineManagerStdio(t, dirA)
	t.Cleanup(aCleanup)
	waitForMethod(t, aMsgs, "engine:ready", 10*time.Second)

	customE2ESeedPeer(t, aStdin, uuidB)

	// Remote add: the whole chain, keyed. The nodeId is addressing and must
	// not survive into the HTTP body; the add fields rebuild there.
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"custom:remote-add","params":`+
		`{"nodeId":"nodeB","name":"lab","url":"`+customE2ENet+`","port":28000,"apiKey":"`+customE2EKey+`"}}`)
	addResp := waitForResponse(t, aMsgs, 30*time.Second)
	require.Nil(t, addResp.Error, "remote add errored: %+v", addResp.Error)
	require.Contains(t, string(addResp.Result), `"keySet":true`,
		"add response must report credential presence")
	require.NotContains(t, string(addResp.Result), customE2EKey,
		"the credential must never ride the response path")

	// The proof the carrier exists for: B's persisted store now carries the
	// entry (key at rest, in the one sanctioned home). The config dir is the
	// isolated root startBrokerWithDirs gave this broker, so the walk cannot
	// stumble into another test's or the developer's store.
	store := ""
	deadline := time.Now().Add(5 * time.Second)
	for store == "" {
		if store = customE2EStorePath(cfgB); store != "" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NotEmpty(t, store, "custom-endpoints.json never appeared on node B")
	persisted, err := os.ReadFile(store)
	require.NoError(t, err)
	require.Contains(t, string(persisted), customE2ENet+":28000")
	require.Contains(t, string(persisted), customE2EKey, "add did not persist at rest")

	// Audit trail: the peer-driven change is logged by op/caller/id — never
	// payload. Presence proves the audit path; the scan proves the policy.
	audit := waitForStderr(t, bStderr, regexp.MustCompile("custom endpoint change from paired node"), 10*time.Second)
	require.NotContains(t, audit, customE2EKey)
	customE2EAssertNoKey(t, bStderr)

	// Remote list: read parity, key-blind (keySet only).
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":2,"method":"custom:remote-list","params":{"nodeId":"nodeB"}}`)
	listResp := waitForResponse(t, aMsgs, 15*time.Second)
	require.Nil(t, listResp.Error, "remote list errored: %+v", listResp.Error)
	require.Contains(t, string(listResp.Result), customE2ENet+":28000")
	require.Contains(t, string(listResp.Result), `"keySet":true`)
	require.NotContains(t, string(listResp.Result), customE2EKey)

	// Remote remove, addressed by the entry id the add reply reported.
	var view struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(addResp.Result, &view))
	require.NotEmpty(t, view.ID)
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":3,"method":"custom:remote-remove","params":`+
		`{"nodeId":"nodeB","id":"`+view.ID+`"}}`)
	removeResp := waitForResponse(t, aMsgs, 15*time.Second)
	require.Nil(t, removeResp.Error, "remote remove errored: %+v", removeResp.Error)
	require.Contains(t, string(removeResp.Result), `"removed":true`)

	// The store converges to empty (either an empty array or the file
	// rewritten without the entry) — the retract half of the contract.
	emptied := false
	emptiedDeadline := time.Now().Add(5 * time.Second)
	for !emptied && time.Now().Before(emptiedDeadline) {
		if data, err := os.ReadFile(store); err == nil && !strings.Contains(string(data), customE2ENet) {
			emptied = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, emptied, "removed entry still present in B's store")
}

// TestCustomRemoteEdit_UntrustedPeer_RefusedAndNothingLands pins the other
// half of the gate: a node with a valid cluster identity that B does not pin
// cannot edit B's custom configuration, and the refusal happens so early that
// not even a trace of the attempted entry reaches B's disk.
func TestCustomRemoteEdit_UntrustedPeer_RefusedAndNothingLands(t *testing.T) {
	if portBusy(14322) || portBusy(14323) {
		t.Skip("engine-manager fixed ports 14322/14323 already in use; skipping")
	}

	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "untrusted-a", "server-b"
	_ = mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB) // A trusts B, so the dial is a real attempt
	writeActiveAdmission(t, dirB)   // B is clustered…
	// …but deliberately holds no pin for A: A must not reach the custom plane.

	cfgB, _ := customE2EStartTarget(t, dirB)

	aStdin, aMsgs, aCleanup := startEngineManagerStdio(t, dirA)
	t.Cleanup(aCleanup)
	waitForMethod(t, aMsgs, "engine:ready", 10*time.Second)

	customE2ESeedPeer(t, aStdin, uuidB)
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"custom:remote-add","params":`+
		`{"nodeId":"nodeB","url":"`+customE2ENet+`","port":28000,"apiKey":"`+customE2EKey+`"}}`)
	resp := waitForResponse(t, aMsgs, 30*time.Second)
	require.NotNil(t, resp.Error, "an unpinned peer was allowed to attempt the edit")
	require.NotContains(t, fmt.Sprint(resp.Error), customE2EKey,
		"the origin's error reply must not repeat the credential")

	// Nothing lands: neither a store file nor an entry in one. (The refusal
	// is at the trust gate, so unlike the local guard this leaves no file.)
	time.Sleep(2 * time.Second)
	if store := customE2EStorePath(cfgB); store != "" {
		data, err := os.ReadFile(store)
		require.NoError(t, err)
		require.NotContains(t, string(data), customE2ENet,
			"an untrusted peer's endpoint reached B's configuration")
	}
}
