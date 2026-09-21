// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	settings "nvpair-shared/enginesettings"
)

const settingsPath = "/v1/engine-settings/"

func (s *controlServer) settingsRoutes(mux *http.ServeMux) {
	for _, method := range []string{"get", "preview", "apply"} {
		mux.HandleFunc(settingsPath+method, s.requirePinCaller(func(w http.ResponseWriter, r *http.Request, caller string) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			if s.exec.settingsParent == nil {
				http.Error(w, "settings coordinator unavailable", 503)
				return
			}
			var p settings.Request
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&p) != nil || decoder.Decode(&struct{}{}) != io.EOF {
				http.Error(w, "invalid settings request", 400)
				return
			}
			// A peer can only address this target node, never forward another hop.
			p.NodeID = ""
			if method != "get" {
				if err := s.exec.RejectRemoteCORSChange(p); err != nil {
					http.Error(w, err.Error(), 403)
					return
				}
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						s.mesh.Refresh()
						if _, ok := s.mesh.VerifyClientPin(r); !ok {
							cancel()
							return
						}
					}
				}
			}()
			result, err := s.exec.settingsParent(ctx, method, p, caller)
			if err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(result)
		}))
	}
	mux.HandleFunc(settingsPath+"events", s.requirePin(s.streamSettings))
}

func (s *controlServer) streamSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.exec.settingsParent == nil {
		http.Error(w, "settings coordinator unavailable", 503)
		return
	}
	ch, closeSub, ok := s.exec.settingsHub.Subscribe()
	if !ok {
		http.Error(w, "too many settings subscriptions", 503)
		return
	}
	defer closeSub()
	w.Header().Set("Content-Type", "application/x-ndjson")
	controller := http.NewResponseController(w)
	send := func(value any) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if json.NewEncoder(w).Encode(value) != nil {
			return false
		}
		return controller.Flush() == nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case snapshot := <-ch:
			s.mesh.Refresh()
			if _, ok := s.mesh.VerifyClientPin(r); !ok {
				return
			}
			if !send(snapshot) {
				return
			}
		case <-ticker.C:
			s.mesh.Refresh()
			if _, ok := s.mesh.VerifyClientPin(r); !ok {
				return
			}
			if !send(nil) {
				return
			}
		}
	}
}

func (m *Manager) remoteSettings(ctx context.Context, msg *Message) {
	var p settings.Request
	if !m.parse(msg, &p) {
		return
	}
	peer, ok := m.peers.lookup(p.NodeID)
	if !ok || m.mesh == nil {
		m.respondOrErr(msg, nil, fmt.Errorf("settings unavailable: device is offline or does not support settings"))
		return
	}
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		m.respondOrErr(msg, nil, err)
		return
	}
	method := strings.TrimSuffix(strings.TrimPrefix(msg.Method, "engine:remote-"), "-settings")
	p.NodeID = ""
	result, err := client.postJSON(ctx, settingsPath+method, p.Engine, p)
	m.respondOrErr(msg, result, err)
}

// Settings are pushed directly by each authority. Discovery polling here only
// manages connection lifetimes; configuration itself is never polled from peers.
func (m *Manager) watchPeerSettings(ctx context.Context) {
	active := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range active {
			cancel()
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.mesh == nil {
				continue
			}
			m.mesh.Refresh()
			m.peers.mu.RLock()
			peers := make(map[string]ecPeer, len(m.peers.peers))
			for k, v := range m.peers.peers {
				peers[k] = v
			}
			m.peers.mu.RUnlock()
			for node, cancel := range active {
				p, exists := peers[node]
				if !exists || !m.mesh.HasPin(p.clusterUUID) {
					cancel()
					delete(active, node)
				}
			}
			for node, peer := range peers {
				if active[node] != nil || len(active) >= 64 || !m.mesh.HasPin(peer.clusterUUID) {
					continue
				}
				streamCtx, cancel := context.WithCancel(ctx)
				active[node] = cancel
				go func(node string) {
					for streamCtx.Err() == nil {
						current, exists := m.peers.lookup(node)
						if !exists {
							return
						}
						m.consumeSettingsStream(streamCtx, current)
						m.exec.notify("engine:settings-disconnected", map[string]string{"nodeId": node})
						select {
						case <-streamCtx.Done():
							return
						case <-time.After(3 * time.Second):
						}
					}
				}(node)
			}
		}
	}
}

func (m *Manager) consumeSettingsStream(parent context.Context, peer ecPeer) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.base+settingsPath+"events", nil)
	if err != nil {
		return
	}
	response, err := client.http.Do(req)
	if err != nil {
		client.forgetAddress()
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return
	}
	watchdog := time.AfterFunc(15*time.Second, cancel)
	defer watchdog.Stop()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		watchdog.Reset(15 * time.Second)
		var snapshots []settings.Snapshot
		if json.Unmarshal(scanner.Bytes(), &snapshots) != nil {
			return
		}
		for _, snapshot := range snapshots {
			if snapshot.Engine != "ollama" && snapshot.Engine != "lmstudio" {
				continue
			}
			snapshot.NodeID = peer.nodeID
			m.exec.notify("engine:settings-changed", snapshot)
		}
	}
}
