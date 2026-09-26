// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
)

// extractLoadedVRAM uses the manifest's loaded-model row filter and name field.
// A bad optional metric must not discard the otherwise usable model inventory.
func extractLoadedVRAM(raw json.RawMessage, spec *ActionResult) map[string]uint64 {
	if spec == nil || spec.VRAMField == "" {
		return nil
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(body[spec.Array], &rows) != nil {
		return nil
	}
	var result map[string]uint64
	for _, row := range rows {
		if spec.Match != nil && !matchRow(row, spec.Match) {
			continue
		}
		var name string
		if json.Unmarshal(row[spec.Field], &name) != nil || name == "" {
			continue
		}
		value := bytes.TrimSpace(row[spec.VRAMField])
		var size uint64
		if len(value) == 0 || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &size) != nil {
			continue
		}
		if result == nil {
			result = make(map[string]uint64)
		}
		result[name] = size
	}
	return result
}
