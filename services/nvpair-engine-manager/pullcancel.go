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
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type activePull struct {
	// id tells this download apart from every other the executor runs,
	// another download of the same model included: a cancelled download still
	// settling and the retry waiting on it name the same engine and model.
	// emitPullProgress stamps it on the download's frames.
	id     uint64
	cancel context.CancelCauseFunc
	done   chan struct{}
	result json.RawMessage
	err    error
	// requested records that someone asked for this download to stop. A
	// context dies for three other reasons — PAIR quitting, a remote
	// initiator's connection dropping, the action timeout elapsing — and none
	// of them mean the user gave up on the bytes already on disk. Only a
	// requested cancellation may delete partial files.
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

// errShuttingDown is why shutdown stopped a download or refused a pull request.
var errShuttingDown = errors.New("engine manager is shutting down")

// downloadShutdownBudget bounds how long shutdown waits for the downloads it
// stopped. The broker gives the whole sweep 5s, the engines stop meanwhile, and
// shutdownDownloadStopGraces have a tool's stop wait 3s at most once shutdown
// begins; the rest covers sending the tool its signals, the runner reporting,
// and the reply. Other stops keep their own bounds, which this one caps. Tests
// shorten it.
var downloadShutdownBudget = 4 * time.Second

type shutdownSignalKey struct{}

// withShutdownSignal carries shutdown, which closes once shutdown begins, on
// the context a download runs on.
func withShutdownSignal(ctx context.Context, shutdown <-chan struct{}) context.Context {
	return context.WithValue(ctx, shutdownSignalKey{}, shutdown)
}

// shutdownSignalFrom returns the channel withShutdownSignal carries on ctx, or
// nil, which never closes, when it carries none.
func shutdownSignalFrom(ctx context.Context) <-chan struct{} {
	shutdown, _ := ctx.Value(shutdownSignalKey{}).(<-chan struct{})
	return shutdown
}

// shutdownSignal returns the channel that closes once shutdown begins. Called
// with pullMu held.
func (e *Executor) shutdownSignal() chan struct{} {
	if e.shutdown == nil {
		e.shutdown = make(chan struct{})
	}
	return e.shutdown
}

// stopDownloads stops every registered download for shutdown and returns them
// for the caller to wait on. Shutdown never marks a stop as one someone asked
// for, so a download only it stopped keeps its partial files for the next
// attempt to resume. Closing the shutdown signal has each tool's stop shorten
// its waits, and partial cleanup stop sampling, a requested cancel's included.
// Called once, by stopAllNow, after it has set shuttingDown, from which point
// trackedPull starts no download.
func (e *Executor) stopDownloads() map[pullKey]*activePull {
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	close(e.shutdownSignal())
	stopping := maps.Clone(e.pulls)
	for _, p := range stopping {
		p.cancel(errShuttingDown)
	}
	return stopping
}

// awaitDownloads waits for the downloads stopDownloads stopped to finish, for
// at most downloadShutdownBudget in all, and logs each still stopping then,
// since engine-manager exits soon after without waiting for it.
func awaitDownloads(stopping map[pullKey]*activePull) {
	ctx, cancel := context.WithTimeout(context.Background(), downloadShutdownBudget)
	defer cancel()
	for key, p := range stopping {
		select {
		case <-p.done:
			continue
		case <-ctx.Done():
		}
		select {
		case <-p.done:
		default:
			slog.Warn("download still stopping at shutdown; engine-manager will not wait for it",
				"engine", key.engine, "model", key.model)
		}
	}
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
	// opID is the client's name for the request, or empty when it gave none.
	// The frames of the download the request runs as carry it, so the client
	// can tell them from another download's of the same model, a retry's
	// included; see registeredDownloadRequests. Fixed when the claim is taken.
	opID string
	// cancelled is set by a cancel that found this request outstanding, and
	// stops it before it starts a download. stopped is the model's download the
	// first such cancel to find one registered stopped along with it, whose
	// outcome the request settles on. Guarded by the executor's pullMu.
	cancelled bool
	stopped   *activePull
	// reported is set once the model's terminal frame goes out while this
	// request is outstanding, so its client has heard how the model's download
	// ended. Guarded by the executor's pullMu.
	reported bool
	// download is the ID of the download the request runs as, once it has
	// registered one, joined one, been stopped along with one by a cancel, or
	// reported its cancellation as a placeholder: the download whose frames
	// are this request's. One a cancel stopped before it started, with no
	// download of its model to settle on, has none unless it reports that
	// cancellation itself. Guarded by the executor's pullMu.
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

// registeredDownloadRequests reports the ID of the download registered for
// engine and model, or zero when none is, and the opIDs of the requests that
// run as it, in the order they were claimed; a request that named none is left
// out. Those are the requests the download's frames speak for: the one that
// started it, whose claim is released only after the download has ended, so
// it is named on every frame; any joined to it, or settling on it after a
// cancel stopped them along with it; and a placeholder's own. A retry waiting
// for a cancelled download to stop runs as no download yet, so that
// download's frames leave it out.
func (e *Executor) registeredDownloadRequests(engine, model string) (id uint64, opIDs []string) {
	key := pullKey{engine: engine, model: model}
	e.pullMu.Lock()
	defer e.pullMu.Unlock()
	p := e.pulls[key]
	if p == nil {
		return 0, nil
	}
	for _, claim := range e.pullClaims[key] {
		if claim.download == p.id && claim.opID != "" {
			opIDs = append(opIDs, claim.opID)
		}
	}
	return p.id, opIDs
}

// opIDPattern is the form of the opId a client names a pull request with.
var opIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// invalidOpIDMessage is the -32602 reply to a pull request whose opId
// validOpID refuses.
const invalidOpIDMessage = "opId must be 1 to 128 characters from A-Z, a-z, 0-9, '.', '_', ':' and '-'"

// validOpID reports whether s can name a pull request: 1 to 128 characters
// from A-Z, a-z, 0-9, ".", "_", ":" and "-". A client names its request so it
// can tell the frames of the download the request runs as from another
// download's of the same model. Telling its own requests apart is the client's
// job, so the name need not be unique.
func validOpID(s string) bool { return opIDPattern.MatchString(s) }

// claimPull marks a pull request accepted and not yet finished. It has to be
// taken where the request is accepted, in the order the client sent it, rather
// than where the download starts. The returned context carries the claim to
// the request's trackedPull, and the returned function releases it once the
// request settles. A request that names no engine and model is not a pull and
// claims nothing. opID is the client's name for the request, empty for none,
// which the caller has checked with validOpID.
func (e *Executor) claimPull(ctx context.Context, engine, model, opID string) (context.Context, func()) {
	if engine == "" || model == "" {
		return ctx, func() {}
	}
	claim := &pullClaim{key: pullKey{engine: engine, model: model}, executor: e, opID: opID}
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
	p := e.pulls[key]
	for _, claim := range e.pullClaims[key] {
		claim.cancelled = true
		if claim.stopped == nil && p != nil {
			claim.stopped = p
			if claim.download == 0 {
				claim.runsAs(key, p)
			}
		}
	}
	if p != nil {
		p.requested.Store(true)
		p.cancel(nil)
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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	p := &activePull{cancel: cancel, done: make(chan struct{})}
	ctx = context.WithValue(ctx, activePullKey{}, p)
	key := pullKey{engine: engine, model: model}
	e.pullMu.Lock()
	ctx = withShutdownSignal(ctx, e.shutdownSignal())
	if e.pulls == nil {
		e.pulls = make(map[pullKey]*activePull)
	}
	for {
		// A cancel that arrived while this request was on its way here stops
		// it before it starts. Only the request's own claim counts, so a cancel
		// aimed at an earlier request for the same model cannot stop this one.
		//
		// A download of the model the cancel stopped along with this request
		// is one the request settles on, as a request joined to it does: once
		// it has stopped, with how it ended. It can finish installing the
		// model as it is stopped, or end on a stop its engine could not
		// confirm, and "cancelled" sooner would deny either.
		//
		// A UI can know frames by engine and model alone, as the desktop app
		// does, so while a download of this model is registered — the
		// cancelled one still settling, or a retry accepted after the cancel —
		// the model's frames are that download's. It reports its own outcome;
		// a "cancelled" frame from this request would repeat it, or tell such
		// a UI that a live retry had ended. Once the model's terminal frame
		// has gone out while this request was outstanding, from a download
		// or from another request stopped like this one, a frame from this
		// request would repeat that too. Either way this request answers
		// "cancelled", never another download's result: a retry that
		// installed the model reports its own success.
		if claim != nil && claim.key == key && claim.cancelled {
			if stopped := claim.stopped; stopped != nil {
				e.pullMu.Unlock()
				return joinPull(ctx, stopped)
			}
			if _, registered := e.pulls[key]; registered || claim.reported {
				e.pullMu.Unlock()
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
			e.pullOutcomeReported(key)
			e.pullMu.Unlock()
			e.emitPullOutcome(engine, model, true, nil)
			e.pullMu.Lock()
			delete(e.pulls, key)
			p.result = cancelledPull()
			close(p.done)
			e.pullMu.Unlock()
			return cancelledPull(), nil
		}
		// Shutdown stops and waits for the downloads registered when it
		// begins, and one registered after would outlive engine-manager. So
		// from then on, unless a cancel stopped it as above, a request starts
		// and joins nothing, and sends no frame. shuttingDown is set before
		// stopDownloads takes pullMu to list the downloads, and this check
		// holds pullMu, so a request either registers before that list is
		// taken or sees the flag. A retry that waited below checks again.
		if e.shuttingDown.Load() {
			e.pullMu.Unlock()
			return nil, fmt.Errorf("pull %q: %w", model, errShuttingDown)
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
		err = pullStopped(ctx, model)
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
	e.pullOutcomeReported(key)
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
// the model's terminal frame has gone out. Called with pullMu held.
func (e *Executor) pullOutcomeReported(key pullKey) {
	for _, claim := range e.pullClaims[key] {
		claim.reported = true
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
