// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// pullcancel.go owns the engine-neutral pull lifecycle: tracking the pull in
// flight for an engine and model, stopping it, serializing an engine's pulls,
// and reporting how each one ended. Removing the files a cancelled download
// left behind is separate — see partials.go for the shared safety test and
// ollamapull.go / lmspull.go for each engine's own.

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
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

// pullKey identifies a download: one engine fetching one model.
type pullKey struct {
	engine string
	model  string
}

func cancelledPull() json.RawMessage { return json.RawMessage(`{"status":"cancelled"}`) }

// pullClaim is one accepted pull request that has not finished.
//
// The pull and its cancel are dispatched onto separate goroutines, so a cancel
// can reach the executor before the download it targets has registered. The
// claim is what the cancel finds instead. It is taken where the request is
// accepted, carried to trackedPull on the request's context, and released when
// the request settles. Each request has its own, so a cancel stops exactly the
// requests that were outstanding when it arrived: a retry accepted after it
// downloads normally, whichever of the two reaches the executor first.
type pullClaim struct {
	key      pullKey
	executor *Executor
	// cancelled is set by a cancel that found this request outstanding, and
	// stops it before it starts a download. Guarded by the executor's pullMu.
	cancelled bool
	once      sync.Once
}

type pullClaimKey struct{}

// claimPull marks a pull request accepted and not yet finished. It has to be
// taken where the request is accepted, in the order the client sent it, rather
// than where the download starts. The returned context carries the claim to
// the request's trackedPull, and the returned function releases it once the
// request settles. A request that names no engine and model is not a pull and
// claims nothing.
func (e *Executor) claimPull(ctx context.Context, engine, model string) (context.Context, func()) {
	if engine == "" || model == "" {
		return ctx, func() {}
	}
	claim := &pullClaim{key: pullKey{engine: engine, model: model}, executor: e}
	e.pullMu.Lock()
	if e.pullClaims == nil {
		e.pullClaims = make(map[pullKey][]*pullClaim)
	}
	e.pullClaims[claim.key] = append(e.pullClaims[claim.key], claim)
	e.pullMu.Unlock()
	return context.WithValue(ctx, pullClaimKey{}, claim), claim.release
}

// release ends the claim. It is safe to call more than once.
func (c *pullClaim) release() {
	c.once.Do(func() {
		e := c.executor
		e.pullMu.Lock()
		defer e.pullMu.Unlock()
		claims := slices.DeleteFunc(e.pullClaims[c.key], func(other *pullClaim) bool { return other == c })
		if len(claims) == 0 {
			delete(e.pullClaims, c.key)
			return
		}
		e.pullClaims[c.key] = claims
	})
}

// CancelModelPull stops the download of model on engine, and acknowledges only
// once the transfer and its cleanup are done. Cancellation must never delete a
// model.
//
// It stops every request for that download outstanding when it arrives: the
// one downloading, any joined to it or queued behind another of the engine's
// downloads, and any accepted but not yet registered, which then never starts.
// A request accepted after the cancel is a retry, and is left alone.
//
// With nothing transferring there is nothing to wait for, so it returns at
// once. That includes finding nothing outstanding at all. Requests are claimed
// on the read loop in the order the client sent them, and the broker relays
// the two in that order, so a cancel that finds no claim arrived after its
// download had already finished.
func (e *Executor) CancelModelPull(ctx context.Context, engine, model string) error {
	key := pullKey{engine: engine, model: model}
	e.pullMu.Lock()
	for _, claim := range e.pullClaims[key] {
		claim.cancelled = true
	}
	p := e.pulls[key]
	if p != nil {
		p.requested.Store(true)
		p.cancel()
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

func (e *Executor) trackedPull(ctx context.Context, engine, model string, run func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	claim, _ := ctx.Value(pullClaimKey{}).(*pullClaim)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &activePull{cancel: cancel, done: make(chan struct{})}
	ctx = context.WithValue(ctx, activePullKey{}, p)
	key := pullKey{engine: engine, model: model}
	e.pullMu.Lock()
	if e.pulls == nil {
		e.pulls = make(map[pullKey]*activePull)
	}
	// A cancel that arrived while this request was on its way here stops it
	// before it starts. Only the request's own claim counts, so a cancel aimed
	// at an earlier request for the same model cannot stop this one.
	//
	// Frames name only the engine and model, so while a download of this model
	// is registered — the cancelled one still settling, or a retry accepted
	// after the cancel — the model's frames are that download's. It reports its
	// own outcome; a "cancelled" frame from this request would repeat it, or
	// tell every UI that a live retry had ended.
	if claim != nil && claim.key == key && claim.cancelled {
		_, registered := e.pulls[key]
		e.pullMu.Unlock()
		if !registered {
			e.emitPullOutcome(engine, model, true, nil)
		}
		return cancelledPull(), nil
	}
	// The vendor caches can share partial files across models. Keep pulls for
	// an engine serialized so cleanup cannot remove another PAIR pull's data.
	var predecessors []<-chan struct{}
	for activeKey, active := range e.pulls {
		if activeKey.engine != engine {
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
	// "Cancelled" is a successful outcome only for a cancellation someone asked
	// for. A context that died on its own leaves a transfer the next attempt
	// resumes, which is a failure to report rather than a request fulfilled.
	cancelled := p.requested.Load() && ctx.Err() == context.Canceled && err == context.Canceled
	if cancelled {
		result, err = cancelledPull(), nil
	}
	// Reported while the download is still registered: a request for the model
	// arriving now joins this outcome, rather than starting a download whose
	// frames this one's would land among.
	e.emitPullOutcome(engine, model, cancelled, err)
	e.pullMu.Lock()
	delete(e.pulls, key)
	p.result, p.err = result, err
	close(p.done)
	e.pullMu.Unlock()
	return result, err
}

// emitPullOutcome sends a download's terminal frame: "success", "cancelled",
// or "error" with the reason. A long download can outlast the client's
// synchronous call, so the reply alone cannot reach a UI that stopped waiting;
// this frame tells every subscriber how the download ended, whichever engine
// ran it, and engines report only the progress before it.
func (e *Executor) emitPullOutcome(engine, model string, cancelled bool, err error) {
	ev := ProgressEvent{Engine: engine, Model: model, Op: "pull", Message: model}
	switch {
	case err != nil:
		ev.Stage, ev.Percent, ev.Message = "error", -1, formatEnginePullError(e.engineDisplayName(engine), err)
	case cancelled:
		ev.Stage = "cancelled"
	default:
		ev.Stage, ev.Percent = "success", 100
	}
	e.emitPullProgress(ev)
}

// joinPull reports the outcome of a download already in flight, so a duplicate
// request is idempotent instead of a failure.
//
// The download keeps running on the context of the request that started it, so
// a joined request shares that request's lifetime: if the owner's connection
// drops or its action timeout elapses, the joined request fails with it. The
// partial files survive that failure, so the next attempt resumes them.
func joinPull(ctx context.Context, p *activePull) (json.RawMessage, error) {
	select {
	case <-p.done:
		return p.result, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
