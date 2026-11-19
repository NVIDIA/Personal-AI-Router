// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// decodeParams unmarshals a JSON-RPC params/result blob into v, treating
// an empty blob as a no-op (leaving v at its zero value). Views use it to
// decode broker notifications and responses.
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// decodeOrLog is decodeParams for a message the view goes on to use whether or
// not it decoded, which is most of them: a push the view folds in, a reply it
// renders. A failure is logged with what was being read — the method, usually
// — so a broker and client that disagree about a shape show up in the Logs tab
// instead of as a screen quietly showing zero values. It reports whether the
// decode succeeded, for a caller that has to act on it.
//
// The payload itself is not logged. Most are small, but some are not — the
// model catalogue is megabytes — and none is needed to see that a shape
// disagrees.
func decodeOrLog(what string, raw json.RawMessage, v any) bool {
	if err := decodeParams(raw, v); err != nil {
		slog.Warn("could not decode a broker message",
			"what", what, "into", fmt.Sprintf("%T", v), "err", err)
		return false
	}
	return true
}
