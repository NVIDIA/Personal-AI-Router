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

	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
)

// startBrokerInDir starts the broker with settings + cluster-manager pointed at
// a caller-supplied config dir (so state persists across a restart when the
// same dir is reused), returning its stdin, stdout frame stream, and a cleanup.
func startBrokerInDir(t *testing.T, configDir string, extraArgs ...string) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
	t.Helper()
	args := append([]string{"--scanner-path", scannerBin, "--cluster-dir", t.TempDir()}, extraArgs...)
	cmd := exec.Command(brokerBin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+configDir,
		"XDG_CONFIG_HOME="+configDir,
		"APPDATA="+configDir,
		"LOCALAPPDATA="+configDir,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "broker stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "broker stdout pipe")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start(), "start broker")
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
			require.True(t, ok, "broker stream closed before response id (%v)", id)
			if msg.Method == "" && idEquals(msg.ID, id) {
				return msg
			}
		case <-timer.C:
			require.FailNow(t, "test expectation failed", "timed out waiting for response id=%d", id)
		}
	}
}

// TestBrokerRestoresClusterIdentityAfterRestart seeds a cluster id into
// nvpair-node-settings through one broker, restarts the broker against the same
// config dir, and asserts cluster:get-node-id reports the restored clusterId
// (rather than the "" a fresh cluster-manager comes up with).
func TestBrokerRestoresClusterIdentityAfterRestart(t *testing.T) {
	configDir := t.TempDir()
	const clusterID = "restore-test-cluster-0001"
	const friendly = "Restore Test Lab"

	// First broker: persist the cluster identity via the settings relay, as a
	// clustered node's UI would have on create/join.
	stdin1, msgs1, cleanup1 := startBrokerInDir(t, configDir,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs1, "app:ready", 15*time.Second)

	setReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":100,"method":"settings/set-cluster-id","params":{"value":%q}}`, clusterID) + "\n"
	{
		_, err := stdin1.Write([]byte(setReq))
		require.NoError(t, err, "write settings/set-cluster-id")
	}
	{
		resp := waitForResponseID(t, msgs1, 100, 10*time.Second)
		require.Nil(t, resp.Error, "settings/set-cluster-id errored")
	}
	nameReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":101,"method":"settings/set-cluster-friendly-name","params":{"value":%q}}`, friendly) + "\n"
	{
		_, err := stdin1.Write([]byte(nameReq))
		require.NoError(t, err, "write settings/set-cluster-friendly-name")
	}
	{
		resp := waitForResponseID(t, msgs1, 101, 10*time.Second)
		require.Nil(t, resp.Error, "settings/set-cluster-friendly-name errored")
	}
	cleanup1()

	// Second broker in the same config dir: the cluster-manager reloads with no
	// clusterId of its own, and the broker must restore it from settings before
	// serving requests.
	stdin2, msgs2, cleanup2 := startBrokerInDir(t, configDir,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	t.Cleanup(cleanup2)
	waitForMethod(t, msgs2, "app:ready", 15*time.Second)

	sendReq(t, stdin2, 200, "cluster:get-node-id")
	resp := waitForResponseID(t, msgs2, 200, 10*time.Second)
	require.Nil(t, resp.Error, "cluster:get-node-id errored")
	var got struct {
		ClusterID string `json:"clusterId"`
	}
	{
		err := json.Unmarshal(resp.Result, &got)
		require.NoError(t, err, "decode cluster:get-node-id")
	}
	require.Equal(t, clusterID, got.ClusterID, "clusterId after restart (%v)", clusterID)
	t.Logf("cluster identity restored after restart: clusterId=%q", got.ClusterID)
}

// clusterIDOf decodes the clusterId from a cluster:get-node-id response.
func clusterIDOf(t *testing.T, resp jsonrpc.Message) string {
	t.Helper()
	require.Nil(t, resp.Error, "response error")
	var r struct {
		ClusterID string `json:"clusterId"`
	}
	{
		err := json.Unmarshal(resp.Result, &r)
		require.NoError(t, err, "decode clusterId")
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
		require.False(t, time.Now().After(deadline), "settings/get-cluster-id never became (%v, %v)", want, last)
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBrokerPersistsClusterLifecycleToSettings exercises the full loop the broker
// closes: cluster:create must persist the new id to nvpair-node-settings (so it
// survives a restart), and cluster:leave must clear it (so the node stays
// unclustered after a restart rather than having the stale id restored).
func TestBrokerPersistsClusterLifecycleToSettings(t *testing.T) {
	if portBusy(14321) {
		t.Skip("cluster-manager inter-node port 14321 already in use; skipping")
	}
	configDir := t.TempDir()

	// 1. Create a cluster; the broker must mirror the new id into settings.
	stdin1, msgs1, cleanup1 := startBrokerInDir(t, configDir,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs1, "app:ready", 15*time.Second)

	sendReq(t, stdin1, 300, "cluster:create")
	createResp := waitForResponseID(t, msgs1, 300, 15*time.Second)
	require.Nil(t, createResp.Error, "cluster:create errored")
	created := clusterIDOf(t, createResp)
	require.NotEqual(t, "", created, "cluster:create returned an empty clusterId")
	waitSettingClusterID(t, stdin1, msgs1, 310, created)
	cleanup1()

	// 2. Restart: the created identity is restored from settings.
	stdin2, msgs2, cleanup2 := startBrokerInDir(t, configDir,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	waitForMethod(t, msgs2, "app:ready", 15*time.Second)
	sendReq(t, stdin2, 320, "cluster:get-node-id")
	{
		got := clusterIDOf(t, waitForResponseID(t, msgs2, 320, 10*time.Second))
		require.Equal(t, created, got, "after restart clusterId (%v, %v)", got, created)
	}

	// 3. Leave: the broker must clear the id in settings.
	sendReq(t, stdin2, 330, "cluster:leave")
	{
		resp := waitForResponseID(t, msgs2, 330, 15*time.Second)
		require.Nil(t, resp.Error, "cluster:leave errored")
	}
	waitSettingClusterID(t, stdin2, msgs2, 340, "")
	cleanup2()

	// 4. Restart: the node stays unclustered (the leave stuck).
	stdin3, msgs3, cleanup3 := startBrokerInDir(t, configDir,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
	)
	t.Cleanup(cleanup3)
	waitForMethod(t, msgs3, "app:ready", 15*time.Second)
	sendReq(t, stdin3, 350, "cluster:get-node-id")
	{
		got := clusterIDOf(t, waitForResponseID(t, msgs3, 350, 10*time.Second))
		require.Equal(t, "", got, "after leave+restart clusterId")
	}
	t.Log("cluster lifecycle persisted: create restored, leave stayed cleared")
}
