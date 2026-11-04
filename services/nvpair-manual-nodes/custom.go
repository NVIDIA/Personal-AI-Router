// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Custom endpoint probing.
//
// The broker owns custom-endpoint configuration (nvpair-ui-broker's
// customendpoints.go is the source of truth); this worker is a probe cache.
// Entries arrive as custom/add requests carrying the broker's persisted form —
// replayed whenever a fresh prober spawns — and leave as custom/updated /
// custom/removed status events that carry no key material. Nothing here
// persists: a restart re-learns the set from the broker.
//
// The probe contract is one GET of the endpoint's model-list path per cycle
// (no /health or /api/tags guessing: the OpenAI model list is the one path
// every Custom target implements). Authorization is injected only when the
// entry carries a key, on that request only. Health follows the manual-node
// failure style: blips are tolerated up to probeFailThreshold consecutive
// misses (401/403 refusals and unparseable bodies count as misses — an
// endpoint that never answers with a parseable model list is not a routable
// endpoint), and recovery is immediate on the first success.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// CustomEntry is this worker's decoding of the broker's
// persistedCustomEndpoint payload (custom/add, add-nudge and replay alike).
// The JSON tags must match that struct exactly — it is marshalled verbatim on
// the wire; a tag drift would silently drop fields at the process boundary.
type CustomEntry struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey,omitempty"`
}

// key is the endpoint's tracking and routing identity: identical logic to the
// broker's persistedCustomEndpoint.key(), so add, remove, replay, and probe
// events all name an endpoint with the same string, no translation step.
func (e CustomEntry) key() string {
	if e.ID != "" {
		return e.ID
	}
	return "custom:" + e.BaseURL
}

// CustomEndpointStatus is the probe result reported to the broker (and on to
// discovery). It deliberately carries no apiKey field: health and the model
// list are the entire event payload. Healthy and Models are reported
// separately because "answering" and "currently routable" are different
// judgements — an empty model list is healthy but offers nothing to route,
// and that second call belongs to the discovery layer.
type CustomEndpointStatus struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	BaseURL string   `json:"baseUrl"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models,omitempty"`
}

type trackedCustom struct {
	entry CustomEntry

	// Snapshot of the last probe result plus the consecutive-miss count,
	// in the same shape as trackedNode: routability survives blips, the
	// flip only fires at probeFailThreshold.
	healthy          bool
	models           []string
	consecutiveFails int
}

// customProbeURL resolves the endpoint's model-list URL from its canonical
// baseUrl. The broker composes baseUrl as scheme://host:port/path, so a base
// that already ends at /v1 came from the user pasting the model-list prefix
// itself; joining there beats producing a /v1/v1 path.
func customProbeURL(baseURL string) string {
	base := strings.TrimSuffix(baseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// addCustom registers an entry as a tracked endpoint, replacing any entry
// already tracked under the same key (the broker's edit flow is
// re-add-with-the-same-key). The replacement starts unprobed: a tracked
// entry's health and model list belong to whatever endpoint produced them.
func (m *Manager) addCustom(entry CustomEntry) (*trackedCustom, CustomEndpointStatus) {
	key := entry.key()
	tc := &trackedCustom{entry: entry}

	m.mu.Lock()
	m.customs[key] = tc
	m.mu.Unlock()

	return tc, CustomEndpointStatus{ID: key, Name: entry.Name, BaseURL: entry.BaseURL}
}

// removeCustom forgets the endpoint tracked under exactly key. The broker
// resolves any accepted alias (key, explicit ID, display name) to the key
// before forwarding, so this stays a plain lookup.
func (m *Manager) removeCustom(key string) bool {
	m.mu.Lock()
	_, exists := m.customs[key]
	if exists {
		delete(m.customs, key)
	}
	m.mu.Unlock()

	if exists {
		m.codec.Notify("custom/removed", CustomEndpointStatus{ID: key})
	}
	return exists
}

// probeCustom runs one probe cycle for a tracked endpoint and reports the
// result. The HTTP exchange happens outside m.mu; the write-back re-reads the
// map so a removal or a re-add during the flight drops these (stale) results
// instead of resurrecting a removed key or polluting the replaced entry.
func (m *Manager) probeCustom(tc *trackedCustom) {
	entry := tc.entry
	healthy, models := m.probeCustomEndpoint(entry)

	m.mu.Lock()
	cur, exists := m.customs[entry.key()]
	if !exists || cur != tc {
		m.mu.Unlock()
		return
	}

	prevHealthy := tc.healthy
	prevModels := tc.models
	if healthy {
		tc.consecutiveFails = 0
		tc.healthy = true
		tc.models = models
	} else {
		tc.consecutiveFails++
		if tc.consecutiveFails >= probeFailThreshold && tc.healthy {
			tc.healthy = false
			tc.models = nil
		}
	}
	changed := prevHealthy != tc.healthy || !sliceEqual(prevModels, tc.models)
	fails := tc.consecutiveFails
	status := CustomEndpointStatus{
		ID:      entry.key(),
		Name:    entry.Name,
		BaseURL: entry.BaseURL,
		Healthy: tc.healthy,
		Models:  tc.models,
	}
	m.mu.Unlock()

	if !changed {
		slog.Debug("custom probe stable",
			"endpoint", status.ID, "healthy", status.Healthy,
			"models", len(status.Models), "consecutive_fails", fails)
		return
	}
	slog.Info("custom endpoint state changed",
		"endpoint", status.ID, "healthy", status.Healthy,
		"models", len(status.Models))
	m.codec.Notify("custom/updated", status)
}

// probeCustomEndpoint performs one model-list request. A 200 whose body is
// the parseable OpenAI envelope is healthy, even when the list is empty;
// everything else — transport failure, any non-200 (401 and 403 refusals
// included), an unparseable body — is a probe miss. That is stricter than
// probeLMStudio's "reachable but unreadable still counts as up": a Custom
// endpoint is defined by answering /v1/models, so unreadable means unknown,
// not up. An https base dials the shared TLS client (system trust store,
// same as an operator-configured TLS manual node).
func (m *Manager) probeCustomEndpoint(entry CustomEntry) (bool, []string) {
	url := customProbeURL(entry.BaseURL)
	client := m.client
	if strings.HasPrefix(url, "https://") {
		client = m.tlsClient
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("custom probe bad url", "endpoint", entry.key(), "err", err)
		return false, nil
	}
	if entry.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+entry.APIKey)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("custom probe failed",
			"endpoint", entry.key(),
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("custom probe non-OK",
			"endpoint", entry.key(), "status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds())
		return false, nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.Debug("custom probe model list unparseable",
			"endpoint", entry.key(), "err", err)
		return false, nil
	}
	models := make([]string, 0, len(result.Data))
	for _, d := range result.Data {
		if d.ID != "" {
			models = append(models, d.ID)
		}
	}
	slog.Debug("custom probe up",
		"endpoint", entry.key(), "models", len(models),
		"duration_ms", time.Since(start).Milliseconds())
	return true, models
}

// customProbeSnapshot answers the broker's on-demand `custom/probe` request:
// it runs one fresh probe cycle for the addressed scope — exactly the endpoint
// named by key, or every tracked endpoint when id is empty — and returns the
// state observed after that cycle, sorted by key for a stable reply.
//
// Probes run outside m.mu like the scheduled cycle. The snapshot re-reads the
// map afterwards, so an entry replaced or removed mid-flight never answers
// with the previous endpoint's verdict or model list: it is either the new
// tracked entry or dropped. The reply is always a non-nil slice; the broker
// merges it as a snapshot of the probed scope, and an omitted array there
// cannot be distinguished from "probed, nothing left".
func (m *Manager) customProbeSnapshot(id string) []CustomEndpointStatus {
	m.mu.Lock()
	targets := make([]*trackedCustom, 0, len(m.customs))
	if id != "" {
		if tc, ok := m.customs[id]; ok {
			targets = append(targets, tc)
		}
	} else {
		for _, tc := range m.customs {
			targets = append(targets, tc)
		}
	}
	m.mu.Unlock()

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].entry.key() < targets[j].entry.key()
	})

	for _, tc := range targets {
		m.probeCustom(tc)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]CustomEndpointStatus, 0, len(targets))
	for _, tc := range targets {
		cur, exists := m.customs[tc.entry.key()]
		if !exists || cur != tc {
			continue
		}
		views = append(views, CustomEndpointStatus{
			ID:      cur.entry.key(),
			Name:    cur.entry.Name,
			BaseURL: cur.entry.BaseURL,
			Healthy: cur.healthy,
			Models:  cur.models,
		})
	}
	return views
}
