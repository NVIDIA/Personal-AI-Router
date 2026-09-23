// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testclient

import (
	"io"
	"net/http"
	"testing"

	"nvpair-shared/httpcon"
)

func TestConnectionCounterCountsDistinctConnections(t *testing.T) {
	client, connections := New(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	const requests = 2
	for i := 0; i < requests; i++ {
		req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		// Request.Close makes the transport reject connection reuse before it
		// handles the response body. This forces a new connection so the
		// counter proves it can count two of them.
		req.Close = true
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		httpcon.DrainAndClose(resp.Body)
	}

	if got := connections.Count(); got != requests {
		t.Fatalf("accepted %d HTTP/1 connections, want %d", got, requests)
	}
}
