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
	"errors"
	"slices"
	"sync"
	"sync/atomic"
)

type activePull struct {
	// id tells this download apart from every other the executor runs,
	// another download of the same model included: a cancelled download still
	// settling and the retry waiting on it name the same engine and model.
	// emitPullProgress stamps it on the download's frames.
	id     uint64
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
	// placeholder marks a request a cancel stopped before it started, held
	// registered only while it reports that, so a retry of the model waits for
	// the frame. It transfers nothing, so no other download queues behind it,
	// and a cancel that finds it answers as one finding nothing would.
	placeholder bool
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

// stopUnconfirmedError is a download failure stopUnconfirmed marked.
type stopUnconfirmedError struct{ err error }

func (e stopUnconfirmedError) Error() string { return e.err.Error() }
func (e stopUnconfirmedError) Unwrap() error { return e.err }

// stopUnconfirmed marks err as an engine's failure to confirm that a download
// it was told to stop has stopped. The vendor may still be transferring, so a
// cancel waiting on that download fails with it — and so does one that asks
// after the download has settled, which would otherwise find nothing
// registered and answer as if it had stopped. The model's next download
// replaces the failure.
func stopUnconfirmed(err error) error { return stopUnconfirmedError{err: err} }

func cancelledPull() json.RawMessage { return json.RawMessage(`{"status":"cancelled"}`) }

// pullResultCancelled reports whether a pull's result is cancelledPull's.
func pullResultCancelled(result json.RawMessage) bool {
	var r struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(result, &r) == nil && r.Status == "cancelled"
}

// pullClaim is one accepted pull request that has not finished.
//
// The pull and its cancel are dispatched onto separate goroutines, so a cancel
// can reach the executor before the download it targets has registered. The
// claim is what the cancel finds instead. It is taken where the request is
// accepted, carried to trackedPull on the request's context, and released when
// the request settles. Each request has its own, so a cancel stops exactly the
// requests that were outstanding when it arrived: a retry accepted after it
// downloads normally, whichever of the two reaches the executor first — once
// the cancelled download has stopped and cleaned up, if it is still doing so.
type pullClaim struct {
	key      pullKey
	executor *Executor
	// cancelled is set by a cancel that found this request outstanding, and
	// stops it before it starts a download. Guarded by the executor's pullMu.
	cancelled bool
	// reported is set once the model's terminal frame goes out while this
	// request is outstanding, so its client has heard how the model's download
	// ended. installed and result keep that download's result if it installed
	// the model. Guarded by the executor's pullMu.
	reported  bool
	installed bool
	result    json.RawMessage
	// download is the ID of the download the request runs as, once it has
	// registered one, joined one, or reported its cancellation as a
	// placeholder: the download whose frames are this request's. A request
	// stopped before it started without a frame of its own has none. Guarded
	// by the executor's pullMu.
	download uint64
	once     sync.Once
}

type pullClaimKey struct{}

// claimedDownload returns a function that reports the ID of the download the
// request claimed on ctx runs as, or zero while it runs as none.
func (e *Executor) claimedDownload(ctx context.Context) func() uint64 {
	claim, _ := ctx.Value(pullClaimKey{}).(*pullClaim)
	return func() uint64 {
		if claim == nil {
			return 0
		}
		e.pullMu.Lock()
		defer e.pullMu.Unlock()
		return claim.download
	}
}

// runsAs records that the claim's request runs as download p of key. Called
// with pullMu held.
func (c *pullClaim) runsAs(key pullKey, p *activePull) {
	if c != nil && c.key == key {
		c.download = p.id
	}
}

// registerPull registers p as the download of key, under an ID of its own,
// and records it as the download claim's request runs as. Called with pullMu
// held.
func (e *Executor) registerPull(key pullKey, p *activePull, claim *pullClaim) {
	e.downloads++
	p.id = e.downloads
	e.pulls[key] = p
	claim.runsAs(key, p)
}

// registeredDownload reports the ID of the download registered for engine and
// model, or zero when none is.
func (e *Executor) registeredDownload(engine, model string) uint64 {
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	if p := e.pulls[pullKey{engine: engine, model: model}]; p != nil {
		return p.id
	}
	return 0
}

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
// A request accepted after the cancel is a retry, and is left alone: it waits
// for the cancelled download to stop and clean up, then downloads.
//
// With nothing transferring there is nothing to wait for, so it returns at
// once. That includes finding nothing outstanding at all. The manager claims
// requests and latches cancels on its read loop, in the order the client sent
// them, and the broker relays the two in that order, so a cancel that finds no
// claim arrived after its download had already finished. The one exception is
// a download whose engine could not confirm it stopped: it may still be
// running, so the cancel reports that failure again rather than an all-clear.
// See stopUnconfirmed.
func (e *Executor) CancelModelPull(ctx context.Context, engine, model string) error {
	return e.latchPullCancel(engine, model)(ctx)
}

// latchPullCancel is the first half of CancelModelPull: it stops the requests
// for model on engine that are outstanding now, and returns the wait for the
// download to stop and clean up. The manager latches a cancel on its read
// loop, where engine:action claims its pulls, so the cancel stops exactly the
// requests read before it, and a retry read after it is left alone however
// the goroutines that carry them are scheduled.
func (e *Executor) latchPullCancel(engine, model string) func(context.Context) error {
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
	unconfirmed := e.stopsUnconfirmed[key]
	e.pullMu.Unlock()
	return func(ctx context.Context) error {
		if p == nil {
			return unconfirmed
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if p.placeholder {
			return unconfirmed
		}
		return p.err
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
	for {
		// A cancel that arrived while this request was on its way here stops
		// it before it starts. Only the request's own claim counts, so a cancel
		// aimed at an earlier request for the same model cannot stop this one.
		//
		// Frames name only the engine and model, so while a download of this
		// model is registered — the cancelled one still settling, or a retry
		// accepted after the cancel — the model's frames are that download's.
		// It reports its own outcome; a "cancelled" frame from this request
		// would repeat it, or tell every UI that a live retry had ended. Once
		// the model's terminal frame has gone out while this request was
		// outstanding, from a download or from another request stopped like
		// this one, a frame from this request would repeat that too. And if
		// that download installed the model, its result is this request's, as
		// though the request had joined it: "cancelled" would say the model is
		// not there.
		if claim != nil && claim.key == key && claim.cancelled {
			if _, registered := e.pulls[key]; registered || claim.reported {
				installed, result := claim.installed, claim.result
				e.pullMu.Unlock()
				if installed {
					return result, nil
				}
				return cancelledPull(), nil
			}
			// Nothing has reported how the model's download ended, so this
			// request does. It stays registered until its frame is out, as a
			// download does while reporting its outcome: a retry accepted after
			// the cancel waits for it rather than sending a first frame this
			// "cancelled" would land after.
			p.placeholder = true
			p.requested.Store(true)
			e.registerPull(key, p, claim)
			e.pullOutcomeReported(key, false, nil)
			e.pullMu.Unlock()
			e.emitPullOutcome(engine, model, true, nil)
			e.pullMu.Lock()
			delete(e.pulls, key)
			p.result = cancelledPull()
			close(p.done)
			e.pullMu.Unlock()
			return cancelledPull(), nil
		}
		active := e.pulls[key]
		if active == nil {
			break
		}
		if !active.requested.Load() {
			// A second request for a download already in flight joins it. An
			// error here would be reported against the live pull's own model,
			// and a terminal error frame disables that row's Cancel button.
			claim.runsAs(key, active)
			e.pullMu.Unlock()
			return joinPull(ctx, active)
		}
		// A cancel is stopping this model's download, and this request was not
		// outstanding when it arrived: it is a retry. Joining would hand it
		// the cancellation it came after, and starting now would put its
		// transfer under the cancelled download's cleanup. So it waits for that
		// download to stop and clean up, sending no frame meanwhile — the
		// model's frames are still the cancelled download's — and then looks
		// again, since a cancel or another request can reach it first.
		e.pullMu.Unlock()
		select {
		case <-active.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		e.pullMu.Lock()
	}
	// The vendor caches can share partial files across models. Keep pulls for
	// an engine serialized so cleanup cannot remove another PAIR pull's data.
	var predecessors []<-chan struct{}
	for activeKey, active := range e.pulls {
		if activeKey.engine == engine && !active.placeholder {
			predecessors = append(predecessors, active.done)
		}
	}
	e.registerPull(key, p, claim)
	// This download supersedes one whose stop was never confirmed: from here a
	// cancel reports this one.
	delete(e.stopsUnconfirmed, key)
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
	// Reported while the download is still registered, so no request for the
	// model starts a download whose frames this one's would land among: one
	// arriving now joins this outcome, or, after a cancel, waits for it.
	e.emitPullOutcome(engine, model, cancelled, err)
	e.pullMu.Lock()
	delete(e.pulls, key)
	e.pullOutcomeReported(key, err == nil && !pullResultCancelled(result), result)
	if errors.As(err, new(stopUnconfirmedError)) {
		if e.stopsUnconfirmed == nil {
			e.stopsUnconfirmed = make(map[pullKey]error)
		}
		e.stopsUnconfirmed[key] = err
	}
	p.result, p.err = result, err
	close(p.done)
	e.pullMu.Unlock()
	return result, err
}

// pullOutcomeReported records, on every request for key still outstanding, that
// the model's terminal frame has gone out, and the result if the download
// installed the model. A later outcome that did not install it leaves an
// earlier install in place, since a cancel never deletes a model. Called with
// pullMu held.
func (e *Executor) pullOutcomeReported(key pullKey, installed bool, result json.RawMessage) {
	for _, claim := range e.pullClaims[key] {
		claim.reported = true
		if installed {
			claim.installed, claim.result = true, result
		}
	}
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

// isPullOutcome reports whether a pull frame's stage is one emitPullOutcome
// ends a download on.
func isPullOutcome(stage string) bool {
	switch stage {
	case "success", "cancelled", "error":
		return true
	}
	return false
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
