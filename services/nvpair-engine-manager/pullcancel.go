// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// pullcancel.go owns the engine-neutral cancellation lifecycle: tracking the
// pull in flight for an engine and model, stopping it, and serializing an
// engine's pulls. Removing the files a cancelled download left behind is
// separate — see partials.go for the shared safety test and ollamapull.go /
// lmspull.go for each engine's own.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type activePull struct {
	cancel context.CancelFunc
	done   chan struct{}
	result json.RawMessage
	err    error
	// requested records that someone asked for this download to stop. A
	// context dies for three other reasons — the manager's run context ending
	// when PAIR quits, a remote initiator's connection dropping, the action
	// timeout elapsing — and none of them mean the user gave up on the bytes
	// already on disk. Only a requested cancellation may delete partial files.
	requested atomic.Bool
}

type activePullKey struct{}

// cancelRequested reports whether the pull running on this context was stopped
// on someone's behalf rather than because the context died underneath it.
func cancelRequested(ctx context.Context) bool {
	p, ok := ctx.Value(activePullKey{}).(*activePull)
	return ok && p.requested.Load()
}

func pullKey(engine, model string) string { return engine + "\x00" + model }

func cancelledPull() json.RawMessage { return json.RawMessage(`{"status":"cancelled"}`) }

// pendingCancelWindow bounds how long a cancel that arrived before its pull
// registered stays armed. Both handlers are dispatched onto their own goroutine
// from the same read loop, so this covers the scheduler running them out of
// order — not a user cancelling a download that never starts.
const pendingCancelWindow = 10 * time.Second

// claimPull marks a pull request accepted and not yet finished, and returns
// the release to run when it settles. The claim is what lets CancelModelPull
// tell a pull that has not registered yet from one that is already over, so it
// has to be taken where the request is accepted rather than where the download
// starts. Claiming something that is not a pull is a no-op.
func (e *Executor) claimPull(engine, model string, isPull bool) func() {
	if !isPull || engine == "" || model == "" {
		return func() {}
	}
	key := pullKey(engine, model)
	e.pullMu.Lock()
	if e.pullClaims == nil {
		e.pullClaims = make(map[string]int)
	}
	e.pullClaims[key]++
	e.pullMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.pullMu.Lock()
			defer e.pullMu.Unlock()
			e.pullClaims[key]--
			if e.pullClaims[key] > 0 {
				return
			}
			delete(e.pullClaims, key)
			// A cancel held for a pull that ended without consuming it has
			// nothing left to stop. Drop it here rather than letting it sit
			// out its window, where a retry would inherit the cancellation.
			delete(e.pendingCancels, key)
		})
	}
}

// CancelModelPull waits for the transfer and its cleanup before acknowledging.
// Cancellation must never delete a model.
//
// A cancel can arrive before its pull has registered, because the pull and the
// cancel are dispatched on separate goroutines. Acknowledging that as "already
// complete" would leave the download running under a UI stuck on "Canceling",
// so it leaves a tombstone the pull consumes instead of starting. That is only
// right while the pull is still owed to someone: an unclaimed engine and model
// has no download to stop, and a tombstone left there would cancel whatever
// pull came next.
func (e *Executor) CancelModelPull(ctx context.Context, engine, model string) error {
	key := pullKey(engine, model)
	e.pullMu.Lock()
	p := e.pulls[key]
	if p != nil {
		p.requested.Store(true)
		p.cancel()
	} else if e.pullClaims[key] > 0 {
		e.armPendingCancel(key)
	}
	e.pullMu.Unlock()
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// armPendingCancel records a cancel for a pull that has not registered yet, and
// drops the entries that have aged out. Callers hold pullMu.
func (e *Executor) armPendingCancel(key string) {
	if e.pendingCancels == nil {
		e.pendingCancels = make(map[string]time.Time)
	}
	now := time.Now()
	for pending, armed := range e.pendingCancels {
		if now.Sub(armed) > pendingCancelWindow {
			delete(e.pendingCancels, pending)
		}
	}
	e.pendingCancels[key] = now
}

// takePendingCancel consumes a tombstone armed for this pull. Callers hold pullMu.
func (e *Executor) takePendingCancel(key string) bool {
	armed, ok := e.pendingCancels[key]
	if !ok {
		return false
	}
	delete(e.pendingCancels, key)
	return time.Since(armed) <= pendingCancelWindow
}

func (e *Executor) trackedPull(ctx context.Context, engine, model string, run func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &activePull{cancel: cancel, done: make(chan struct{})}
	ctx = context.WithValue(ctx, activePullKey{}, p)
	key := pullKey(engine, model)
	e.pullMu.Lock()
	if e.pulls == nil {
		e.pulls = make(map[string]*activePull)
	}
	if e.takePendingCancel(key) {
		e.pullMu.Unlock()
		return cancelledPull(), nil
	}
	// The vendor caches can share partial files across models. Keep pulls for
	// an engine serialized so cleanup cannot remove another PAIR pull's data.
	var predecessors []<-chan struct{}
	for activeKey, active := range e.pulls {
		if !strings.HasPrefix(activeKey, engine+"\x00") {
			continue
		}
		if activeKey == key {
			// A second request for a download already in flight joins it. An
			// error here would be reported against the live pull's own model,
			// and a terminal error frame disables that row's Cancel button.
			e.pullMu.Unlock()
			return joinPull(ctx, active)
		}
		predecessors = append(predecessors, active.done)
	}
	e.pulls[key] = p
	e.pullMu.Unlock()
	var result json.RawMessage
	var err error
	if len(predecessors) > 0 {
		e.emitPullProgress(ProgressEvent{Engine: engine, Model: model, Op: "pull", Stage: "queued"})
	}
waitForPredecessors:
	for _, done := range predecessors {
		select {
		case <-done:
		case <-ctx.Done():
			break waitForPredecessors
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		result, err = run(ctx)
	}
	e.pullMu.Lock()
	delete(e.pulls, key)
	// "Cancelled" is a successful outcome only for a cancellation someone asked
	// for. A context that died on its own leaves a transfer the next attempt
	// resumes, which is a failure to report rather than a request fulfilled.
	if p.requested.Load() && ctx.Err() == context.Canceled && err == context.Canceled {
		result, err = cancelledPull(), nil
	}
	p.result, p.err = result, err
	close(p.done)
	e.pullMu.Unlock()
	return result, err
}

// joinPull reports the outcome of a download already in flight, so a duplicate
// request is idempotent instead of a failure.
func joinPull(ctx context.Context, p *activePull) (json.RawMessage, error) {
	select {
	case <-p.done:
		return p.result, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
