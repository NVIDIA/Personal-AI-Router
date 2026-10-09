// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// TestWriteClusterIdentityFrame pins the wire form of the push node-info decodes.
// node-info reads its stdin as newline-delimited JSON-RPC, so a missing newline or
// a renamed method silently stops membership reaching it — and the only symptom
// would be peers going on suppressing an invite for a node that has left.
func TestWriteClusterIdentityFrame(t *testing.T) {
	test := func(name, clusterUUID string) {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			var mu sync.Mutex
			require.NoError(t, writeClusterIdentityFrame(&mu, &buf, clusterUUID), "write")

			line := buf.String()
			assert.True(t, strings.HasSuffix(line, "\n"), "frame is not newline-terminated; node-info reads line-delimited frames")
			assert.Equal(t, 1, strings.Count(line, "\n"), "frame must contain exactly one newline")

			var frame struct {
				JSONRPC string                        `json:"jsonrpc"`
				Method  string                        `json:"method"`
				ID      json.RawMessage               `json:"id"`
				Params  noderec.ClusterIdentityParams `json:"params"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &frame), "decode frame %q", line)
			assert.Equal(t, "2.0", frame.JSONRPC)
			assert.Equal(t, noderec.MethodSetClusterIdentity, frame.Method, "method")
			// A notification, not a request: node-info's stdout is drained to
			// io.Discard, so an id-bearing frame would strand a reply.
			assert.Empty(t, frame.ID, "the push must be a notification")
			assert.Equal(t, clusterUUID, frame.Params.ClusterUUID, "clusterUuid")
			// The field must be on the wire even when empty, since that is how a
			// departure is expressed.
			assert.Contains(t, line, `"clusterUuid"`, "frame omitted clusterUuid")
		})
	}
	test("a principal", "our-principal")
	// A departure is the value peers are waiting for, so it is sent like any
	// other rather than skipped as empty.
	test("a departure", "")
}
