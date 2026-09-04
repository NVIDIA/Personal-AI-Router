// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"

	"nvpair-shared/noderec"
)

// openaiEngine binds one OpenAI-compatible engine's discovery service key to
// the engine-manager engine name that owns its models and tags its workloads.
// The two are separate contracts and are deliberately not derived from each
// other: the key is the mDNS TXT spelling, the name is what engine-manager's
// modelsByEngine map and the workload wire use.
type openaiEngine struct {
	Service noderec.ServiceKey
	Name    string
}

// openaiEngines is every engine this proxy fronts, in the order it resolves
// them. The order is the deterministic tie-break for a model id that more than
// one engine on the same node advertises — first listed wins — so routing and
// workload attribution can never depend on map iteration order.
//
// Every entry speaks the same OpenAI HTTP surface, which is what lets one proxy
// serve them all: the engines differ in how they are installed and managed, not
// in how a request is forwarded.
var openaiEngines = []openaiEngine{
	{Service: noderec.ServiceLMStudio, Name: "lmstudio"},
	{Service: noderec.ServiceVLLM, Name: "vllm"},
}

// subscribedServices is the discovery:subscribe service list: every engine key
// this proxy routes for.
func subscribedServices() []noderec.ServiceKey {
	out := make([]noderec.ServiceKey, 0, len(openaiEngines))
	for _, e := range openaiEngines {
		out = append(out, e.Service)
	}
	return out
}

// engineNames returns the engine-manager names in resolution order.
func engineNames() []string {
	out := make([]string, 0, len(openaiEngines))
	for _, e := range openaiEngines {
		out = append(out, e.Name)
	}
	return out
}

// isOpenAIEngine reports whether a name is one this proxy fronts. It gates the
// engine field on node/set-local-backend and node/add-manual so a typo is
// refused at the boundary rather than silently creating an unroutable entry.
func isOpenAIEngine(name string) bool {
	return slices.Contains(engineNames(), name)
}

// engineForModel returns the engine on this node that owns a model id, using
// openaiEngines order as the tie-break, and "" when none advertises it. A node
// with no per-engine attribution at all (a peer whose scanner has not enriched
// it yet) resolves to "" and is attributed by the caller.
func engineForModel(modelsByEngine map[string][]string, model string) string {
	if model == "" {
		return ""
	}
	for _, e := range openaiEngines {
		if slices.Contains(modelsByEngine[e.Name], model) {
			return e.Name
		}
	}
	return ""
}

// unionModels flattens a per-engine model map into one sorted, de-duplicated
// list — the node's whole OpenAI-reachable inventory, which is what eligibility
// filtering and the aggregated /v1/models answer both work from. Sorted so a
// node's Models list is stable across snapshots and cannot spuriously trip the
// node/updated diff.
func unionModels(modelsByEngine map[string][]string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, e := range openaiEngines {
		for _, m := range modelsByEngine[e.Name] {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// candidateEngine is the engine a forwarded request is attributed to. A
// candidate resolved from a model always names its owner; one resolved with no
// routing model (a control call) falls back to the first engine, matching the
// order routing itself resolves in.
func candidateEngine(c candidate) string {
	if c.engine != "" {
		return c.engine
	}
	return fallbackWorkloadEngine
}

// appendCandidate adds a resolved candidate unless its backend host is already
// claimed or resolves back to this proxy. The self check is defensive: a local
// backend must never point at our own listener, which would loop.
func appendCandidate(out []candidate, seenHost map[string]bool, c candidate, selfPort int) []candidate {
	if c.url == nil {
		return out
	}
	if isSelfTarget(c.url, selfPort) {
		slog.Debug("resolveCandidates: skipping self-target node",
			"node_id", c.id, "engine", c.engine, "target", c.url.Host, "self_port", selfPort)
		return out
	}
	if seenHost[c.url.Host] {
		return out
	}
	seenHost[c.url.Host] = true
	return append(out, c)
}

// nodeEngineFor names the engine on a node that owns the routing model, or ""
// when there is no routing model or the node carries no attribution for it. A
// manual node has exactly one engine and answers with it directly.
func (p *Proxy) nodeEngineFor(n Node, model string) string {
	if n.Engine != "" {
		return n.Engine
	}
	return engineForModel(n.ModelsByEngine, model)
}

// selfTargets are the local loopback engines a candidate for this node resolves
// to. With a routing model it is the one engine that serves it — read from the
// engines themselves, which is authoritative where the node's own discovery
// entry can lag. Without one (a control call or the aggregated model list) it is
// every healthy local engine, so nothing this node serves is hidden.
func (p *Proxy) selfTargets(n Node, model, discoveredEngine string) []localEngineTarget {
	backends := p.localBackends()
	if len(backends) == 0 {
		slog.Debug("resolveCandidates: no local backend for self", "node_id", n.ID)
		return nil
	}
	if model == "" {
		return backends
	}
	engine := engineForModel(p.localModelsByEngine(context.Background(), backends), model)
	if engine == "" {
		engine = discoveredEngine
	}
	for _, b := range backends {
		if b.Engine == engine {
			return []localEngineTarget{b}
		}
	}
	// The model is advertised for this node but no local engine claims it. Do not
	// guess an engine that would answer 404: leave the node out and let another
	// owner, or the no-owner rejection, decide.
	slog.Debug("resolveCandidates: no local engine owns the requested model", "node_id", n.ID)
	return nil
}

// modelListTransportError marks a model-list failure that happened before any
// response arrived, so the caller can drop the confirmed address for that
// target. A protocol-level failure (bad status, unparseable body) proves the
// address is reachable and must not.
type modelListTransportError struct{ err error }

func (e modelListTransportError) Error() string { return e.err.Error() }
func (e modelListTransportError) Unwrap() error { return e.err }

func isTransportError(err error) bool {
	var t modelListTransportError
	return errors.As(err, &t)
}

// fetchModelList reads one OpenAI /v1/models endpoint and returns its records
// with their ids. It is shared by the cluster-wide aggregation (over a peer's
// mTLS proxy) and the ingress aggregation (over this node's loopback engines),
// so both apply the same size cap and the same strict envelope validation.
func fetchModelList(ctx context.Context, client *http.Client, target *url.URL) ([]modelListItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, modelListTransportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxModelListBytes {
		return nil, fmt.Errorf("model list exceeds %d bytes", maxModelListBytes)
	}
	var envelope struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil {
		return nil, fmt.Errorf("upstream response has no data array")
	}
	items := make([]modelListItem, 0, len(*envelope.Data))
	for _, raw := range *envelope.Data {
		var identity struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			return nil, fmt.Errorf("invalid model record: %w", err)
		}
		if identity.ID == "" {
			return nil, fmt.Errorf("model record has no id")
		}
		items = append(items, modelListItem{key: identity.ID, raw: raw})
	}
	return items, nil
}
