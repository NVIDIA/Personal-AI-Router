// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/engines"
	"nvpair-shared/jsonrpc"
)

var (
	proxyBin        string
	errorsBin       string
	nodeInfoBin     string
	scannerBin      string
	nodeSettingsBin string
	brokerBin       string
	workloadMgrBin  string
	engineMgrBin    string
	manualNodesBin  string
	clusterMgrBin   string
	schedulerBin    string
	testsConfigBase string
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "nvpair-tests-*")
	if err != nil {
		log.Fatalf("failed to create temp dir: %v", err)
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	proxyBin = filepath.Join(tmpDir, "nvpair-proxy"+ext)
	errorsBin = filepath.Join(tmpDir, "nvpair-errors"+ext)
	nodeInfoBin = filepath.Join(tmpDir, "nvpair-node-info"+ext)
	scannerBin = filepath.Join(tmpDir, "nvpair-node-scanner"+ext)
	brokerBin = filepath.Join(tmpDir, "nvpair-ui-broker"+ext)
	nodeSettingsBin = filepath.Join(tmpDir, "nvpair-node-settings"+ext)
	workloadMgrBin = filepath.Join(tmpDir, "nvpair-workload-manager"+ext)
	engineMgrBin = filepath.Join(tmpDir, "nvpair-engine-manager"+ext)
	manualNodesBin = filepath.Join(tmpDir, "nvpair-manual-nodes"+ext)
	clusterMgrBin = filepath.Join(tmpDir, "nvpair-cluster-manager"+ext)
	schedulerBin = filepath.Join(tmpDir, "nvpair-job-scheduler"+ext)

	// One process fronts every engine, with a facade enabled per engine, so
	// both the Ollama and LM Studio bridge tests run against this build.
	log.Println("building nvpair-proxy...")
	if err := goBuild(filepath.Join("..", "nvpair-proxy"), proxyBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-proxy: %v", err)
	}

	log.Println("building nvpair-errors...")
	if err := goBuild(filepath.Join("..", "nvpair-errors"), errorsBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-errors: %v", err)
	}

	log.Println("building nvpair-node-info...")
	if err := goBuild(filepath.Join("..", "nvpair-node-info"), nodeInfoBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-node-info: %v", err)
	}

	// The broker spawns nvpair-node-scanner, so the cross-process broker test
	// needs both binaries. The broker is pointed at scannerBin via
	// --scanner-path so it doesn't depend on a sibling-file layout.
	log.Println("building nvpair-node-scanner...")
	if err := goBuild(filepath.Join("..", "nvpair-node-scanner"), scannerBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-node-scanner: %v", err)
	}

	log.Println("building nvpair-ui-broker...")
	if err := goBuild(filepath.Join("..", "nvpair-ui-broker"), brokerBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-ui-broker: %v", err)
	}

	log.Println("building nvpair-node-settings...")
	if err := goBuild(filepath.Join("..", "nvpair-node-settings"), nodeSettingsBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-node-settings: %v", err)
	}

	// The broker supervises nvpair-workload-manager, so the workload-interop
	// test needs its binary too. The broker is pointed at workloadMgrBin
	// via --workload-manager-path.
	log.Println("building nvpair-workload-manager...")
	if err := goBuild(filepath.Join("..", "nvpair-workload-manager"), workloadMgrBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-workload-manager: %v", err)
	}

	// The broker now also supervises engine-manager, manual-nodes, and
	// cluster-manager, so the broker-supervision tests need their binaries
	// (pointed at via --engine-manager-path / --manual-nodes-path /
	// --cluster-manager-path).
	log.Println("building nvpair-engine-manager...")
	if err := goBuild(filepath.Join("..", "nvpair-engine-manager"), engineMgrBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-engine-manager: %v", err)
	}

	log.Println("building nvpair-manual-nodes...")
	if err := goBuild(filepath.Join("..", "nvpair-manual-nodes"), manualNodesBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-manual-nodes: %v", err)
	}

	log.Println("building nvpair-cluster-manager...")
	if err := goBuild(filepath.Join("..", "nvpair-cluster-manager"), clusterMgrBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-cluster-manager: %v", err)
	}

	// The broker supervises nvpair-job-scheduler (pointed at via --scheduler-path);
	// the scheduler-interop test also drives its binary directly.
	log.Println("building nvpair-job-scheduler...")
	if err := goBuild(filepath.Join("..", "nvpair-job-scheduler"), schedulerBin); err != nil {
		os.RemoveAll(tmpDir)
		log.Fatalf("build nvpair-job-scheduler: %v", err)
	}

	// Every child binary inherits this environment, so its persisted state
	// (ports, workloads, settings) lands under tmpDir instead of the
	// developer's real config. These are the variables appdir resolves
	// through on each platform. They are set after the builds, because go
	// derives its module and build caches from these variables.
	testsConfigBase = filepath.Join(tmpDir, "config")
	for _, key := range []string{"XDG_CONFIG_HOME", "HOME", "APPDATA", "LOCALAPPDATA"} {
		if err := os.Setenv(key, testsConfigBase); err != nil {
			os.RemoveAll(tmpDir)
			log.Fatalf("set %s: %v", key, err)
		}
	}

	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}

func goBuild(srcDir, output string) error {
	cmd := exec.Command("go", "build", "-o", output, ".")
	cmd.Dir = srcDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- JSON-RPC helpers ---

// The on-wire JSON-RPC frame and error envelope are the shared
// nvpair-shared/jsonrpc types (jsonrpc.Message / jsonrpc.RPCError), so the
// integration tests parse exactly what production emits.

func startMsgReader(r io.Reader) <-chan jsonrpc.Message {
	ch := make(chan jsonrpc.Message, 64)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
		for scanner.Scan() {
			var msg jsonrpc.Message
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				ch <- msg
			}
		}
	}()
	return ch
}

func waitForMethod(t *testing.T, ch <-chan jsonrpc.Message, method string, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before receiving %q", method)
			}
			if msg.Method == method {
				return msg
			}
		case <-timer.C:
			t.Fatalf("timed out (%s) waiting for method %q", timeout, method)
		}
	}
	return jsonrpc.Message{}
}

func waitForResponse(t *testing.T, ch <-chan jsonrpc.Message, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before receiving response")
			}
			if msg.ID != nil && msg.Method == "" {
				return msg
			}
		case <-timer.C:
			t.Fatal("timed out waiting for JSON-RPC response")
		}
	}
	return jsonrpc.Message{}
}

// TestLMStudioFacadeChildPersistsUnderPrivateBase proves a real proxy child
// writes its persisted LM Studio port under the TestMain config base, so no
// cross-process test can clobber the developer's saved port.
func TestLMStudioFacadeChildPersistsUnderPrivateBase(t *testing.T) {
	cmd := exec.Command(proxyBin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	msgs := startMsgReader(stdout)

	sendLine(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "facade/enable",
		"params": map[string]any{
			"engine":              "lmstudio",
			"port":                freePort(t),
			"ignorePersistedPort": true,
		},
	})
	if resp := waitForResponse(t, msgs, 10*time.Second); resp.Error != nil {
		t.Fatalf("facade/enable failed: %v", resp.Error)
	}

	persistedPort := freePort(t)
	sendLine(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  engines.AddressMethod("lmstudio", "set-port"),
		"params":  map[string]any{"port": persistedPort},
	})
	if resp := waitForResponse(t, msgs, 5*time.Second); resp.Error != nil {
		t.Fatalf("lmstudio set-port failed: %v", resp.Error)
	}

	path, err := appdir.Path("lmstudio-proxy-port.json")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(testsConfigBase, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("lmstudio port path %q is outside test config base %q", path, testsConfigBase)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted lmstudio port: %v", err)
	}
	var saved struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("parse persisted lmstudio port: %v", err)
	}
	if saved.Port != persistedPort {
		t.Fatalf("persisted lmstudio port = %d, want %d", saved.Port, persistedPort)
	}
}
