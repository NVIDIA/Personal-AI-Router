// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"nvpair-shared/modelrequest"
	"sync"
)

var errRequestEngineDisabled = errors.New("engine is disabled")
var errRequestModelNotCached = errors.New("model is not downloaded")

type modelPreparations struct {
	mu      sync.Mutex
	pending map[string]context.CancelFunc
}

// prepareRequestedModel follows the same launch transaction as an explicit
// Load, with stricter admission: inference cannot enable an engine or download
// an uncached model. The transaction rechecks admission under its operation lock.
func (e *Executor) prepareRequestedModel(ctx context.Context, engine, model string) (int, error) {
	st, err := e.state(engine)
	if err != nil {
		return 0, err
	}
	st.mu.Lock()
	allowed := st.plat != nil && st.plat.Runtime.WaitForModel
	st.mu.Unlock()
	flag := modelLaunchFlag(st.manifest)
	if !allowed || flag == "" {
		return 0, errors.New("this engine does not support request-triggered model loading")
	}
	params, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return 0, err
	}
	if _, err := e.changeLaunchModel(ctx, engine, &ActionLaunchModel{Flag: flag}, params, true); err != nil {
		return 0, err
	}
	state, err := e.Status(engine)
	if err != nil {
		return 0, err
	}
	if !state.Running || !state.Healthy {
		return 0, fmt.Errorf("engine %q was stopped before model preparation finished", engine)
	}
	return state.Port, nil
}

func (m *Manager) handleModelPreparation(ctx context.Context, msg *Message) bool {
	if msg.Method == modelrequest.EngineCancelAll && msg.IsNotification() {
		m.preparations.mu.Lock()
		for _, cancel := range m.preparations.pending {
			cancel()
		}
		m.preparations.mu.Unlock()
		return true
	}
	if !msg.IsNotification() {
		return false
	}
	if msg.Method != modelrequest.EngineRequest && msg.Method != modelrequest.EngineCancel {
		return false
	}
	var p modelrequest.Preparation
	if json.Unmarshal(msg.Params, &p) != nil || p.ID == "" || p.Engine == "" {
		return true
	}
	m.preparations.mu.Lock()
	if msg.Method == modelrequest.EngineCancel {
		cancel := m.preparations.pending[p.ID]
		m.preparations.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return true
	}
	if _, duplicate := m.preparations.pending[p.ID]; duplicate {
		m.preparations.mu.Unlock()
		return true
	}
	if len(m.preparations.pending) >= 64 {
		m.preparations.mu.Unlock()
		_ = m.codec.Notify(modelrequest.EngineReply, modelrequest.Reply{ID: p.ID, Engine: p.Engine, Error: "too many pending model preparations"})
		return true
	}
	prepareCtx, cancel := context.WithTimeout(ctx, modelrequest.Budget)
	if m.preparations.pending == nil {
		m.preparations.pending = make(map[string]context.CancelFunc)
	}
	m.preparations.pending[p.ID] = cancel
	m.preparations.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { m.preparations.mu.Lock(); delete(m.preparations.pending, p.ID); m.preparations.mu.Unlock() }()
		port, err := m.exec.prepareRequestedModel(prepareCtx, p.Engine, p.Model)
		reply := modelrequest.Reply{ID: p.ID, Engine: p.Engine, Port: port}
		if err != nil {
			reply.Error = err.Error()
			reply.Status = http.StatusUnprocessableEntity
			if errors.Is(err, errRequestEngineDisabled) {
				reply.Status = http.StatusConflict
			}
			if errors.Is(err, errRequestModelNotCached) {
				reply.Status = http.StatusNotFound
			}
		}
		m.exec.pokeLoaded()
		_ = m.codec.Notify(modelrequest.EngineReply, reply)
	}()
	return true
}
