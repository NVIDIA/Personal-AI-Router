// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import "encoding/json"

// decodeParams unmarshals a JSON-RPC params/result blob into v, treating
// an empty blob as a no-op (leaving v at its zero value). Views use it to
// decode broker notifications and responses.
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
