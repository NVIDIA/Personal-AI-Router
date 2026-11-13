// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"nvpair-shared/modelrequest"
	"strings"
	"sync"
	"sync/atomic"
)

// modelRequestGate admits concurrent requests for one model, with FIFO groups
// for other models. A switch waits until every response for the current model
// has finished. Cancelled waiters never hold the queue open.
type modelRequestGate struct {
	mu     sync.Mutex
	active int
	model  string
	queue  []*modelWaiter
}

type modelWaiter struct {
	model   string
	ready   chan struct{}
	granted bool
}

func (g *modelRequestGate) schedule() {
	if g.active != 0 || len(g.queue) == 0 {
		return
	}
	g.model = g.queue[0].model
	for len(g.queue) > 0 && g.queue[0].model == g.model {
		waiter := g.queue[0]
		g.queue = g.queue[1:]
		g.active++
		waiter.granted = true
		close(waiter.ready)
	}
}

func (g *modelRequestGate) acquire(ctx context.Context, model string) (func(), error) {
	waiter := &modelWaiter{model: model, ready: make(chan struct{})}
	g.mu.Lock()
	if g.active > 0 && g.model == model && len(g.queue) == 0 {
		g.active++
		waiter.granted = true
		close(waiter.ready)
	} else {
		g.queue = append(g.queue, waiter)
		g.schedule()
	}
	g.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if waiter.granted {
				g.active--
			} else {
				for i, w := range g.queue {
					if w == waiter {
						g.queue = append(g.queue[:i], g.queue[i+1:]...)
						break
					}
				}
			}
			g.schedule()
		})
	}
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}

type modelRequestRelay struct {
	mu      sync.Mutex
	next    atomic.Uint64
	pending map[string]chan modelrequest.Reply
}

func (f *facade) prepareRequestedModel(ctx context.Context, model string) error {
	relay := &f.modelRequests
	id := fmt.Sprintf("%s:%s:%d", f.host.runID, f.profile.Name, relay.next.Add(1))
	reply := make(chan modelrequest.Reply, 1)
	relay.mu.Lock()
	if len(relay.pending) >= 64 {
		relay.mu.Unlock()
		return errors.New("too many pending model preparations")
	}
	if relay.pending == nil {
		relay.pending = make(map[string]chan modelrequest.Reply)
	}
	relay.pending[id] = reply
	relay.mu.Unlock()
	defer func() { relay.mu.Lock(); delete(relay.pending, id); relay.mu.Unlock() }()
	request := modelrequest.Preparation{ID: id, Engine: f.profile.Name, Model: model}
	if err := f.host.codec.Notify(modelrequest.Request, request); err != nil {
		return err
	}
	select {
	case result := <-reply:
		if result.Error != "" {
			return modelPreparationFailure{status: result.Status, message: result.Error}
		}
		if result.Port != f.currentLocalBackend().Port {
			return errors.New("engine port changed while preparing the model")
		}
		return nil
	case <-ctx.Done():
		_ = f.host.codec.Notify(modelrequest.Cancel, request)
		return ctx.Err()
	}
}

func (p *Proxy) handleModelPrepared(msg *Message) bool {
	if msg.Method == modelrequest.Reset && msg.IsNotification() {
		for _, f := range p.enabledFacades() {
			f.modelRequests.mu.Lock()
			for id, pending := range f.modelRequests.pending {
				select {
				case pending <- modelrequest.Reply{ID: id, Engine: f.profile.Name, Error: "engine manager restarted during model preparation", Status: http.StatusServiceUnavailable}:
				default:
				}
			}
			f.modelRequests.mu.Unlock()
		}
		return true
	}
	if msg.Method != modelrequest.ReplyMethod || !msg.IsNotification() {
		return false
	}
	var reply modelrequest.Reply
	if json.Unmarshal(msg.Params, &reply) != nil {
		return true
	}
	f := p.facadeFor(reply.Engine)
	if f == nil {
		return true
	}
	f.modelRequests.mu.Lock()
	pending := f.modelRequests.pending[reply.ID]
	f.modelRequests.mu.Unlock()
	if pending != nil {
		select {
		case pending <- reply:
		default:
		}
	}
	return true
}

// prepareModelTransport holds admission until the entire response body is
// consumed or closed, including streaming responses and failed forwards.
type prepareModelTransport struct {
	next   http.RoundTripper
	facade *facade
	model  string
}

func (t prepareModelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(request.Context(), modelrequest.Budget)
	defer cancel()
	release, err := t.facade.modelGate.acquire(ctx, t.model)
	if err != nil {
		return nil, err
	}
	if err := t.facade.prepareRequestedModel(ctx, t.model); err != nil {
		release()
		var failure modelPreparationFailure
		if !errors.As(err, &failure) {
			return nil, err
		}
		status := failure.status
		if status < 400 || status > 599 {
			status = http.StatusServiceUnavailable
		}
		body, encodeErr := json.Marshal(map[string]string{"error": failure.message})
		if encodeErr != nil {
			return nil, encodeErr
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	}
	response, err := t.next.RoundTrip(request)
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &modelResponseBody{ReadCloser: response.Body, release: release}
	return response, nil
}

type modelResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *modelResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}
func (b *modelResponseBody) Close() error { defer b.release(); return b.ReadCloser.Close() }

func (f *facade) requestTransport(next http.RoundTripper, r *http.Request, model string) http.RoundTripper {
	if !f.profile.LoadOnRequest || model == "" || !isInferenceRequest(f.profile, r.Method, r.URL.Path) {
		return next
	}
	backend := f.currentLocalBackend()
	if !backend.LoadOnRequest {
		return next
	}
	target, ok := f.localBackendTarget()
	if !ok || target.Host != r.URL.Host {
		return next
	}
	return prepareModelTransport{next: next, facade: f, model: model}
}

func (f *facade) requestTransportForCandidate(c candidate, r *http.Request, model string) http.RoundTripper {
	next := http.RoundTripper(f.host.candidateTransport(c))
	if f.profile.LoadOnRequest && isInferenceRequest(f.profile, r.Method, r.URL.Path) {
		next = f.host.modelLoadingTransport(c)
	}
	copy := r.Clone(r.Context())
	copy.URL.Scheme = c.url.Scheme
	copy.URL.Host = c.url.Host
	return f.requestTransport(next, copy, model)
}

// Cold loading happens before a peer can return inference headers. Keep its
// bounded preparation allowance separate from the ordinary inference deadline,
// while retaining one pooled transport per peer and the existing pin checks.
func (p *Proxy) modelLoadingTransport(c candidate) http.RoundTripper {
	if c.peerUUID != "" && (p.mesh == nil || !p.mesh.HasPin(c.peerUUID)) {
		return modelTransportFailure{err: errors.New("model-loading peer is no longer trusted")}
	}
	base := p.candidateTransport(c)
	p.transportMu.Lock()
	defer p.transportMu.Unlock()
	if transport := p.modelTransports[c.peerUUID]; transport != nil {
		return transport
	}
	transport := base.Clone()
	transport.ResponseHeaderTimeout = modelrequest.Budget + proxyResponseTimeout
	if p.modelTransports == nil {
		p.modelTransports = make(map[string]*http.Transport)
	}
	p.modelTransports[c.peerUUID] = transport
	return transport
}

type modelPreparationFailure struct {
	status  int
	message string
}

func (e modelPreparationFailure) Error() string { return e.message }

type modelTransportFailure struct{ err error }

func (t modelTransportFailure) RoundTrip(r *http.Request) (*http.Response, error) { return nil, t.err }

func availableModelItems(models []string) ([]modelListItem, error) {
	items := make([]modelListItem, 0, len(models))
	for _, model := range models {
		raw, err := json.Marshal(map[string]any{"id": model, "object": "model", "owned_by": "pair", "created": 0})
		if err != nil {
			return nil, err
		}
		items = append(items, modelListItem{key: model, raw: raw})
	}
	return items, nil
}

// Available inventory is operational metadata from engine-manager. Browser
// origins still use the native engine's response and CORS decision.
func (f *facade) serveLocalAvailableModels(w http.ResponseWriter, r *http.Request) bool {
	if !f.profile.LoadOnRequest || !f.currentLocalBackend().LoadOnRequest || r.Header.Get("Origin") != "" {
		return false
	}
	route, ok := f.profile.routeFor(r.Method, r.URL.Path)
	if !ok || route.Role != roleModelListOpenAIGET {
		return false
	}
	models, known := f.discovery.LocalModels()
	if !known {
		return false
	}
	items, err := availableModelItems(models)
	if err != nil {
		writeIngressError(w, http.StatusInternalServerError, "model-inventory", "model inventory is unavailable")
		return true
	}
	records := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		records = append(records, item.raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": records})
	return true
}

// Lifecycle-only updates omit Models and retain the last authoritative cached
// capability set. This lets ON and OFF take effect before the UI state event,
// independently of the next model-list poll. Empty snapshots explicitly clear it.
func (f *facade) setLocalModelInventory(inventory modelrequest.LocalInventory) error {
	f.localInventoryMu.Lock()
	defer f.localInventoryMu.Unlock()
	selfPort, _ := f.selfAddresses()
	if inventory.EnginePort < 1 || inventory.EnginePort > 65535 || inventory.EnginePort == selfPort {
		return errors.New("invalid local engine port")
	}
	if inventory.Models == nil && f.localInventory != nil {
		inventory.Models = append([]string{}, f.localInventory.Models...)
	} else {
		inventory.Models = append([]string{}, inventory.Models...)
	}
	previous := f.currentLocalBackend()
	if err := f.setLocalBackend(localBackend{Engine: f.profile.Name, Host: "127.0.0.1", Port: inventory.EnginePort, Healthy: inventory.Enabled && previous.Healthy && previous.Port == inventory.EnginePort, LoadOnRequest: inventory.Enabled}); err != nil {
		return err
	}
	f.localInventory = &inventory
	advertised := inventory.Models
	if !inventory.Enabled {
		advertised = []string{}
	}
	f.discovery.SetLocal(Node{ID: inventory.ID, Host: "127.0.0.1", Port: selfPort, Models: advertised, Addresses: []string{"127.0.0.1"}})
	return nil
}
