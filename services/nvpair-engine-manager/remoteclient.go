// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// remoteclient.go is the outbound half of remote engine management: dialing a
// peer's ec surface over pin-based mTLS. It presents this node's cluster leaf
// and pins the peer's exact server cert (nvpair-shared/clustertrust), so a call
// only succeeds against a peer we hold a pin for — the same trust decision the
// server side enforces with a 403.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	remoteResponseHeaderTimeout      = 30 * time.Second
	remoteReadyResponseHeaderTimeout = 11 * time.Minute
)

// remoteClient talks to one peer's ec surface.
type remoteClient struct {
	http *http.Client
	// readyHTTP serves endpoints whose peer may withhold response headers during
	// engine readiness or Ollama model loading. See waitsForEngineReadiness.
	readyHTTP *http.Client
	base      string // https://<ip>:<port>
	forget    func()
}

// waitsForEngineReadiness reports whether a peer may only answer after an
// engine is healthy or an Ollama model is loaded, which the ordinary 30s
// response-header budget cannot cover.
//
// Cutting such a call off is worse than slow. The initiator's cancellation
// propagates into the peer's in-flight handler, so bringUpCommand tears the
// engine back down — for a delete that means the files are gone, the engine is
// left stopped, and the caller is told the delete failed.
func waitsForEngineReadiness(path, engine string) bool {
	// controlDeletePath: LM Studio's delete_model declares restart_after, so the
	// peer replies only after the post-delete restart is ready.
	if path == controlLoadPath {
		return engine == "ollama"
	}
	return path == controlStartPath || path == controlDeletePath
}

func newRemoteHTTPClient(base *http.Transport, responseHeaderTimeout time.Duration) *http.Client {
	tr := base.Clone()
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: tr}
}

// remoteClient builds a pinned-mTLS client for a target peer. It fails when this
// node isn't clustered or the peer isn't a pinned cluster member. Membership is
// re-derived per call, so remote management becomes available as soon as this
// node joins a cluster and stops the moment it leaves.
func (m *Manager) remoteClient(ctx context.Context, peer ecPeer) (*remoteClient, error) {
	m.mesh.Refresh()
	if !m.mesh.Clustered() {
		return nil, fmt.Errorf("this node is not clustered; remote engine management is unavailable")
	}
	m.remoteHTTP.DropUnpinned()
	m.readyHTTP.DropUnpinned()
	httpClient, ok := m.remoteHTTP.Client(peer.clusterUUID)
	if !ok {
		return nil, fmt.Errorf("node %q is not a pinned cluster peer", peer.nodeID)
	}
	readyClient, ok := m.readyHTTP.Client(peer.clusterUUID)
	if !ok {
		return nil, fmt.Errorf("node %q is not a pinned cluster peer", peer.nodeID)
	}
	// No total client timeout: install/pull run for minutes, bounded on the
	// server by the executor's action ceiling. ResponseHeaderTimeout still
	// guards a peer that accepts the connection but never replies (ordinary
	// vs readiness budgets live on the two pools).
	candidates := make([]string, 0, len(peer.addresses))
	for _, address := range peer.addresses {
		candidates = append(candidates, net.JoinHostPort(address, strconv.Itoa(peer.port)))
	}
	hostport := m.addrs.ChooseWithin(ctx, peer.nodeID, candidates)
	return &remoteClient{
		http:      httpClient,
		readyHTTP: readyClient,
		base:      "https://" + hostport,
		forget:    func() { m.addrs.Forget(peer.nodeID) },
	}, nil
}

func (c *remoteClient) forgetAddress() {
	if c.forget != nil {
		c.forget()
	}
}

// getEngines fetches the peer's engine status list (GET /v1/engines), returning
// the raw {"engines":[...]} object.
func (c *remoteClient) getEngines(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+controlEnginesPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.forgetAddress()
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("remote engines: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.RawMessage(data), nil
}

// postJSON POSTs body to a non-streaming ec endpoint and returns the raw JSON
// response (e.g. an EngineStatus from start/stop).
func (c *remoteClient) postJSON(ctx context.Context, path, engine string, body any) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.http
	if waitsForEngineReadiness(path, engine) && c.readyHTTP != nil {
		client = c.readyHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		c.forgetAddress()
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("remote %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.RawMessage(data), nil
}

// stream POSTs body to a streaming ec endpoint and consumes its NDJSON frames,
// calling onProgress for each progress frame and returning the terminal result
// frame. An error frame (or a stream that ends without a result) is an error.
func (c *remoteClient) stream(ctx context.Context, path string, body any, onProgress func(streamFrame)) (streamFrame, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return streamFrame{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return streamFrame{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.forgetAddress()
		return streamFrame{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return streamFrame{}, fmt.Errorf("remote %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	dec := json.NewDecoder(resp.Body)
	var result streamFrame
	haveResult := false
	for {
		var f streamFrame
		if derr := dec.Decode(&f); derr != nil {
			if derr == io.EOF {
				break
			}
			return streamFrame{}, fmt.Errorf("remote %s: decode frame: %w", path, derr)
		}
		switch f.Type {
		case "progress":
			if onProgress != nil {
				onProgress(f)
			}
		case "result":
			result = f
			haveResult = true
		case "error":
			return streamFrame{}, fmt.Errorf("%s", f.Message)
		}
	}
	if !haveResult {
		return streamFrame{}, fmt.Errorf("remote %s: stream ended without a result", path)
	}
	return result, nil
}
