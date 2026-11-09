// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httpcon provides helpers for HTTP connections.
package httpcon

import "io"

const maxDrainBytes = 1 << 20

// DrainAndClose discards a bounded response body before closing it so normal
// HTTP/1 responses reach EOF and their connections can return to the transport
// pool. Without this, connections cannot be reused and this can lead to socket
// exhaustion. The caller is responsible for bounding stalled reads with a
// request context or client timeout.
func DrainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
	_ = body.Close()
}
