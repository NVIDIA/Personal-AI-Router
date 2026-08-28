// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"nvpair-shared/appdir"
)

const proxyPortFile = "lmstudio-proxy-port.json"

const (
	defaultProxyPort       = 1234
	legacyDefaultProxyPort = 1235
)

type persistedPort struct {
	Port int `json:"port"`
}

func chooseStartupPort(flagPort int, ignorePersisted bool, persisted int, hasPersisted bool) int {
	if ignorePersisted || !hasPersisted || persisted == legacyDefaultProxyPort {
		return flagPort
	}
	return persisted
}

func proxyPortPath() (string, error) {
	return appdir.Path(proxyPortFile)
}

// loadPersistedPort returns the previously chosen proxy port, if a valid one
// was saved. Any error (no file, bad JSON, out-of-range) reports "none" so
// startup falls back to the --port flag / default.
func loadPersistedPort() (int, bool) {
	path, err := proxyPortPath()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var pp persistedPort
	if err := json.Unmarshal(data, &pp); err != nil {
		return 0, false
	}
	if pp.Port < 1 || pp.Port > 65535 {
		return 0, false
	}
	return pp.Port, true
}

// savePersistedPort atomically writes the chosen port (tmp + rename) so a
// crash mid-write can't leave a truncated file behind.
func savePersistedPort(port int) error {
	path, err := proxyPortPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(persistedPort{Port: port})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
