// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
)

// HardwarePreviewConcurrency limits public HTTP reads across all nodes and
// addresses in a collector process. Workers also use it to bound active nodes.
const HardwarePreviewConcurrency = 4

// HardwarePreviewTimeout bounds a read, including waiting for reader capacity.
// Scanner uses the same budget for a complete address sweep.
const HardwarePreviewTimeout = 1500 * time.Millisecond

var previewSlots = make(chan struct{}, HardwarePreviewConcurrency)

// Public HTTP is retained solely for hardware previews before pairing. This
// client does not follow redirects or use a proxy for local-network discovery.
var previewClient = &http.Client{
	Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ReadHardwarePreview reads display-only hardware. No reported identity or
// registry metadata survives this boundary, and callers must bind the result to
// their current unverified record without promoting it or feeding scheduling.
func ReadHardwarePreview(ctx context.Context, host string, port int) (NodeInfo, bool) {
	ctx, cancel := context.WithTimeout(ctx, HardwarePreviewTimeout)
	defer cancel()
	select {
	case previewSlots <- struct{}{}:
		defer func() { <-previewSlots }()
	case <-ctx.Done():
		return NodeInfo{}, false
	}

	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v1/node-info"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return NodeInfo{}, false
	}
	started := time.Now()
	response, err := previewClient.Do(request)
	if err != nil {
		return NodeInfo{}, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return NodeInfo{}, false
	}
	info, err := DecodeNodeInfo(response.Body)
	if err != nil {
		return NodeInfo{}, false
	}
	info.HostUUID, info.ClusterUUID, info.Peer = "", nil, nil
	return info.Aged(time.Since(started)), true
}
