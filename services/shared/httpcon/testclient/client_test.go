// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testclient

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/httpcon"
)

func TestConnectionCounterCountsDistinctConnections(t *testing.T) {
	client, connections := New(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, "ok")
		assert.NoError(t, err, "write response")
	}))

	const requests = 2
	for i := 0; i < requests; i++ {
		req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		require.NoError(t, err)
		// Request.Close makes the transport reject connection reuse before it
		// handles the response body. This forces a new connection so the
		// counter proves it can count two of them.
		req.Close = true
		resp, err := client.Do(req)
		require.NoError(t, err)
		httpcon.DrainAndClose(resp.Body)
	}

	assert.Equal(t, int32(requests), connections.Count())
}
