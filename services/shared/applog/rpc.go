// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"encoding/json"
	"fmt"
)

// SetLevelMethod is the canonical JSON-RPC method name subprocesses expose
// (and the UI invokes) to change the active log level at runtime.
const SetLevelMethod = "log/set-level"

// SetLevelParams is the shape of the JSON-RPC params for SetLevelMethod.
type SetLevelParams struct {
	Level string `json:"level"`
}

// HandleSetLevelParams parses a log/set-level params blob, applies the level
// via SetLevel, and returns the resolved level string or an error.
//
// Subprocesses should call this from their JSON-RPC handler for
// SetLevelMethod messages (both notifications and requests).
func HandleSetLevelParams(params json.RawMessage) (string, error) {
	var p SetLevelParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return "", fmt.Errorf("invalid params: %w", err)
		}
	}
	lvl, err := ParseLevel(p.Level)
	if err != nil {
		return "", err
	}
	SetLevel(lvl)
	return LevelString(), nil
}
