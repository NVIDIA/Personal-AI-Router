// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for the broker migrating the per-user data dir before any
// worker reads it: a node upgraded from a release that used an earlier directory
// name must come up with the node identity it already had, not a freshly minted
// one that peers would see as a new, unpaired node.
package tests

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// dataBase is the platform base appdir resolves under the environment
// startBrokerInDir sets.
func dataBase(configDir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(configDir, "Library", "Application Support")
	}
	return configDir
}

func TestBrokerMigratesPreviousDataDirBeforeWorkersStart(t *testing.T) {
	const seededUUID = "11111111-2222-4333-8444-555555555555"
	configDir := t.TempDir()
	base := dataBase(configDir)
	previous := filepath.Join(base, "Nvidia Corporation", "Personal AI Router")
	current := filepath.Join(base, "Nvidia Corporation", "NVIDIA PAIR")

	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatalf("mkdir previous data dir: %v", err)
	}
	seed, err := json.Marshal(map[string]any{"node_uuid": seededUUID, "created_at": time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode node-id.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(previous, "node-id.json"), seed, 0o600); err != nil {
		t.Fatalf("write previous node-id.json: %v", err)
	}

	_, msgs, cleanup := startBrokerInDir(t, configDir)
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	cleanup()

	data, err := os.ReadFile(filepath.Join(current, "node-id.json"))
	if err != nil {
		t.Fatalf("read migrated node-id.json: %v", err)
	}
	var got struct {
		NodeUUID string `json:"node_uuid"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode migrated node-id.json: %v", err)
	}
	if got.NodeUUID != seededUUID {
		t.Fatalf("node_uuid = %q, want the pre-upgrade %q", got.NodeUUID, seededUUID)
	}
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous data dir should be gone after migration (stat err: %v)", err)
	}
}
