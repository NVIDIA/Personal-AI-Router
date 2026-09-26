// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
)

// lookupField retains literal-key precedence and supports nested vendor state.
func lookupField(row map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	if value, ok := row[field]; ok {
		return value, true
	}
	parts := strings.Split(field, ".")
	for i, part := range parts {
		value, ok := row[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return value, true
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) != nil {
			return nil, false
		}
		row = nested
	}
	return nil, false
}
