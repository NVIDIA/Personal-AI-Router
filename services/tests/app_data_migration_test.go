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
	"sync"
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

const seededUUID = "11111111-2222-4333-8444-555555555555"

// seedPreviousNodeID writes a node identity into the pre-rename data directory
// and returns the previous and current directories.
func seedPreviousNodeID(t *testing.T, configDir string) (string, string) {
	t.Helper()
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
	return previous, current
}

func TestBrokerMigratesPreviousDataDirBeforeWorkersStart(t *testing.T) {
	configDir := t.TempDir()
	previous, current := seedPreviousNodeID(t, configDir)

	_, msgs, cleanup := startBrokerInDir(t, configDir)
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	cleanup()

	assertMigratedNodeID(t, current)
	if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous data dir should be gone after migration (stat err: %v)", err)
	}
}

func TestBrokerWaitsForMigrationLockBeforeWorkersStart(t *testing.T) {
	configDir := t.TempDir()
	_, current := seedPreviousNodeID(t, configDir)
	lock := filepath.Join(dataBase(configDir), "Nvidia Corporation", ".nvpair-data-migration.lock")
	if err := os.WriteFile(lock, []byte("desktop-app"), 0o600); err != nil {
		t.Fatalf("write migration lock: %v", err)
	}

	_, msgs, stopBroker := startBrokerInDir(t, configDir)
	var once sync.Once
	cleanup := func() { once.Do(stopBroker) }
	defer cleanup()
	held := time.After(2 * time.Second)
	for waiting := true; waiting; {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker exited while another process held the migration lock")
			}
			if msg.Method == "app:ready" {
				t.Fatal("broker became ready while another process held the migration lock")
			}
		case <-held:
			waiting = false
		}
	}
	if _, err := os.Stat(filepath.Join(current, "node-id.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node-id.json exists while the lock is held (stat err: %v)", err)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatalf("release migration lock: %v", err)
	}
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	cleanup()

	assertMigratedNodeID(t, current)
}

func assertMigratedNodeID(t *testing.T, current string) {
	t.Helper()
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
}
