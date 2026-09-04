// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for the broker restoring a node's cluster identity on
// startup: nvpair-cluster-manager persists its member roster but not the clusterId
// (which lives in nvpair-node-settings), so after a restart the broker must
// re-inject it via cluster:set-identity. Without that, cluster:get-node-id would
// report "not clustered" while the roster still lists members.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// startBrokerInDir starts the broker with settings + cluster-manager pointed at
// a caller-supplied config dir (so state persists across a restart when the
// same dir is reused), returning its stdin, stdout frame stream, and a cleanup.
// extraEnv (may be nil) carries env entries for the broker process, e.g.
// service-port overrides; the remaining args are broker CLI args.
func startBrokerInDir(t *testing.T, configDir string, extraEnv []string, extraArgs ...string) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
	t.Helper()
	args := append([]string{"--scanner-path", scannerBin, "--cluster-dir", t.TempDir()}, extraArgs...)
	cmd := exec.Command(brokerBin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+configDir,
		"XDG_CONFIG_HOME="+configDir,
		"APPDATA="+configDir,
		"LOCALAPPDATA="+configDir,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("broker stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("broker stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	msgs := startMsgReader(stdout)
	cleanup := func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	return stdin, msgs, cleanup
}

// waitForResponseID scans for the JSON-RPC response with the given id.
func waitForResponseID(t *testing.T, msgs <-chan jsonrpc.Message, id int, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatalf("broker stream closed before response id=%d", id)
			}
			if msg.Method == "" && idEquals(msg.ID, id) {
				return msg
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for response id=%d", id)
		}
	}
}

// TestBrokerRestoresClusterIdentityAfterRestart seeds a cluster id into
// nvpair-node-settings through one broker, restarts the broker against the same
// config dir, and asserts cluster:get-node-id reports the restored clusterId
// (rather than the "" a fresh cluster-manager comes up with).
func TestBrokerRestoresClusterIdentityAfterRestart(t *testing.T) {
	svcEnv, _ := freeServicePortEnv(t)
	configDir := t.TempDir()
	const clusterID = "restore-test-cluster-0001"
	const friendly = "Restore Test Lab"

	// First broker: persist the cluster identity via the settings relay, as a
	// clustered node's UI would have on create/join.
	stdin1, msgs1, cleanup1 := startBrokerInDir(t, configDir, svcEnv,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs1, "app:ready", 15*time.Second)

	setReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":100,"method":"settings/set-cluster-id","params":{"value":%q}}`, clusterID) + "\n"
	if _, err := stdin1.Write([]byte(setReq)); err != nil {
		t.Fatalf("write settings/set-cluster-id: %v", err)
	}
	if resp := waitForResponseID(t, msgs1, 100, 10*time.Second); resp.Error != nil {
		t.Fatalf("settings/set-cluster-id errored: %d %s", resp.Error.Code, resp.Error.Message)
	}
	nameReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":101,"method":"settings/set-cluster-friendly-name","params":{"value":%q}}`, friendly) + "\n"
	if _, err := stdin1.Write([]byte(nameReq)); err != nil {
		t.Fatalf("write settings/set-cluster-friendly-name: %v", err)
	}
	if resp := waitForResponseID(t, msgs1, 101, 10*time.Second); resp.Error != nil {
		t.Fatalf("settings/set-cluster-friendly-name errored: %d %s", resp.Error.Code, resp.Error.Message)
	}
	cleanup1()

	// Second broker in the same config dir: the cluster-manager reloads with no
	// clusterId of its own, and the broker must restore it from settings before
	// serving requests.
	stdin2, msgs2, cleanup2 := startBrokerInDir(t, configDir, svcEnv,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	t.Cleanup(cleanup2)
	waitForMethod(t, msgs2, "app:ready", 15*time.Second)

	sendReq(t, stdin2, 200, "cluster:get-node-id")
	resp := waitForResponseID(t, msgs2, 200, 10*time.Second)
	if resp.Error != nil {
		t.Fatalf("cluster:get-node-id errored: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var got struct {
		ClusterID string `json:"clusterId"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode cluster:get-node-id: %v\nraw: %s", err, resp.Result)
	}
	if got.ClusterID != clusterID {
		t.Fatalf("clusterId after restart = %q, want %q (identity was not restored)", got.ClusterID, clusterID)
	}
	t.Logf("cluster identity restored after restart: clusterId=%q", got.ClusterID)
}

// clusterIDOf decodes the clusterId from a cluster:get-node-id response.
func clusterIDOf(t *testing.T, resp jsonrpc.Message) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("response error: %d %s", resp.Error.Code, resp.Error.Message)
	}
	var r struct {
		ClusterID string `json:"clusterId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("decode clusterId: %v\nraw: %s", err, resp.Result)
	}
	return r.ClusterID
}

// waitSettingClusterID polls settings/get-cluster-id through the broker until it
// equals want. The broker persists cluster:identity-changed asynchronously, so a
// single read can race the write.
func waitSettingClusterID(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, startID int, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	id := startID
	var last string
	for {
		sendReq(t, stdin, id, "settings/get-cluster-id")
		resp := waitForResponseID(t, msgs, id, 5*time.Second)
		id++
		var r struct {
			Value string `json:"value"`
		}
		if resp.Error == nil && json.Unmarshal(resp.Result, &r) == nil {
			last = r.Value
			if r.Value == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("settings/get-cluster-id never became %q (last=%q)", want, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBrokerPersistsClusterLifecycleToSettings exercises the full loop the broker
// closes: cluster:create must persist the new id to nvpair-node-settings (so it
// survives a restart), and cluster:leave must clear it (so the node stays
// unclustered after a restart rather than having the stale id restored).
func TestBrokerPersistsClusterLifecycleToSettings(t *testing.T) {
	svcEnv, _ := freeServicePortEnv(t)
	configDir := t.TempDir()

	// 1. Create a cluster; the broker must mirror the new id into settings.
	stdin1, msgs1, cleanup1 := startBrokerInDir(t, configDir, svcEnv,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs1, "app:ready", 15*time.Second)

	sendReq(t, stdin1, 300, "cluster:create")
	createResp := waitForResponseID(t, msgs1, 300, 15*time.Second)
	if createResp.Error != nil {
		t.Fatalf("cluster:create errored: %d %s", createResp.Error.Code, createResp.Error.Message)
	}
	created := clusterIDOf(t, createResp)
	if created == "" {
		t.Fatal("cluster:create returned an empty clusterId")
	}
	waitSettingClusterID(t, stdin1, msgs1, 310, created)
	cleanup1()

	// 2. Restart: the created identity is restored from settings.
	stdin2, msgs2, cleanup2 := startBrokerInDir(t, configDir, svcEnv,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs2, "app:ready", 15*time.Second)
	sendReq(t, stdin2, 320, "cluster:get-node-id")
	if got := clusterIDOf(t, waitForResponseID(t, msgs2, 320, 10*time.Second)); got != created {
		t.Fatalf("after restart clusterId = %q, want %q", got, created)
	}

	// 3. Leave: the broker must clear the id in settings.
	sendReq(t, stdin2, 330, "cluster:leave")
	if resp := waitForResponseID(t, msgs2, 330, 15*time.Second); resp.Error != nil {
		t.Fatalf("cluster:leave errored: %d %s", resp.Error.Code, resp.Error.Message)
	}
	waitSettingClusterID(t, stdin2, msgs2, 340, "")
	cleanup2()

	// 4. Restart: the node stays unclustered (the leave stuck).
	stdin3, msgs3, cleanup3 := startBrokerInDir(t, configDir, svcEnv,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	t.Cleanup(cleanup3)
	waitForMethod(t, msgs3, "app:ready", 15*time.Second)
	sendReq(t, stdin3, 350, "cluster:get-node-id")
	if got := clusterIDOf(t, waitForResponseID(t, msgs3, 350, 10*time.Second)); got != "" {
		t.Fatalf("after leave+restart clusterId = %q, want empty (leave did not stick)", got)
	}
	t.Log("cluster lifecycle persisted: create restored, leave stayed cleared")
}
