// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/appdir"
	"nvpair-ui-broker/workloadstore"
)

var uiBrokerTestConfigBase string

func TestMain(m *testing.M) {
	base, err := os.MkdirTemp("", "nvpair-ui-broker-config-*")
	if err != nil {
		panic(err)
	}
	uiBrokerTestConfigBase = base
	keys := []string{"XDG_CONFIG_HOME", "HOME", "APPDATA", "LOCALAPPDATA"}
	type savedEnv struct {
		value string
		set   bool
	}
	previous := make(map[string]savedEnv, len(keys))
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		previous[key] = savedEnv{value: value, set: set}
	}
	cleanup := func() {
		for _, key := range keys {
			prior := previous[key]
			if prior.set {
				_ = os.Setenv(key, prior.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
		_ = os.RemoveAll(base)
	}
	for _, key := range keys {
		if err := os.Setenv(key, base); err != nil {
			cleanup()
			panic(err)
		}
	}

	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestWorkloadHistoryPersistsUnderPrivateBase(t *testing.T) {
	path, err := appdir.Path("workloads-history.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Broker{workloads: workloadstore.New()}
	b.workloads.WithPersistence(path)
	if err := b.workloads.Load(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	info, err := json.Marshal(map[string]any{
		"id":             "test-history",
		"originatedFrom": "test-node",
		"state":          "completed",
		"scheduledOn":    "test-node",
		"createdAt":      now - 1,
		"completedAt":    now,
		"model":          "test-model",
		"engine":         "ollama",
	})
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]json.RawMessage{"workloadInfo": info})
	if err != nil {
		t.Fatal(err)
	}
	if !b.applyWorkloadEvent("workloads:upsert", params, false, time.Time{}) {
		t.Fatal("terminal workload event was not applied")
	}
	if err := b.workloads.Flush(); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(uiBrokerTestConfigBase, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("workload history path %q is outside test config base %q", path, uiBrokerTestConfigBase)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("persisted workload history: %v", err)
	}
}
