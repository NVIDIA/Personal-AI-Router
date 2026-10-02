// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"
)

// jobEvents is one inference request's workload record and the lifecycle
// events it emits. Each change, its seq and its notification happen under one
// lock, so events leave the process in seq order: the request goroutine and the
// disconnect watcher cannot overtake each other on the wire. The broker's store
// and the workload-manager's peers read seq as the order the events were made
// in.
//
// A job is queued while it waits, either for a node or for a slot on the node
// it was sent to, and running once that node's engine has started it. Each
// attempt to send it somewhere is numbered, so a late report from an attempt
// the request has moved on from changes nothing.
//
// Lock order is the job's lock, then the codec's write lock inside emit.
// Nothing that holds the codec lock takes a job's lock.
//
// A nil *jobEvents is a request that is not tracked as a workload; every method
// is then a no-op.
type jobEvents struct {
	mu         sync.Mutex
	wl         Workload
	seq        int64
	attempt    uint64
	terminated bool
	emit       func(method string, wl Workload)
}

// newJobEvents records an admitted request and emits it as workload:submitted.
// wl carries no Seq; numbering starts here.
func newJobEvents(wl Workload, emit func(method string, wl Workload)) *jobEvents {
	j := &jobEvents{wl: wl, emit: emit}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.notifyLocked(workloadSubmittedMethod)
	return j
}

// repoint records where the job is placed: a node id while an attempt is in
// flight, empty between attempts. The scheduler counts pending work by
// scheduledOn, so clearing it is what stops a node we have given up on from
// still looking busy. A move leaves the job queued, with no StartedAt, until the
// node it moved to starts it. A move to the node the job already waits on, or
// any move after finish, emits nothing.
func (j *jobEvents) repoint(nodeID string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.repointLocked(nodeID)
}

// dispatch begins a new attempt on nodeID, re-pointing the job there, and
// returns the attempt's number for markRunning.
func (j *jobEvents) dispatch(nodeID string) uint64 {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.attempt++
	j.repointLocked(nodeID)
	return j.attempt
}

func (j *jobEvents) repointLocked(nodeID string) {
	if j.terminated || (j.wl.State == "queued" && j.wl.ScheduledOn == nodeID) {
		return
	}
	j.wl.State = "queued"
	j.wl.StartedAt = nil
	j.wl.ScheduledOn = nodeID
	j.notifyLocked(workloadSubmittedMethod)
}

// markRunning records that attempt's request holds a slot on nodeID. It applies
// only while attempt is the current one and the job still waits on nodeID, and
// never after finish.
func (j *jobEvents) markRunning(attempt uint64, nodeID string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.terminated || attempt != j.attempt || j.wl.State != "queued" || j.wl.ScheduledOn != nodeID {
		return
	}
	j.runLocked(nodeID)
}

// start records the commit on nodeID: the engine is producing content, so the
// job is running there. It emits nothing when the job already reads running on
// nodeID, which keeps the StartedAt a slot gave it. It is skipped after finish,
// so a late commit cannot resurrect a workload that has already ended.
func (j *jobEvents) start(nodeID string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.terminated || (j.wl.State == "running" && j.wl.ScheduledOn == nodeID) {
		return
	}
	j.runLocked(nodeID)
}

func (j *jobEvents) runLocked(nodeID string) {
	startedMs := time.Now().UnixMilli()
	j.wl.State = "running"
	j.wl.StartedAt = &startedMs
	j.wl.ScheduledOn = nodeID
	j.notifyLocked(workloadStartedMethod)
}

// finish emits the terminal transition, at most once: the request's deferred
// reporter and the disconnect watcher can both reach it. Only "completed" gets
// its own method; every other terminal state, including "cancelled", rides
// workload:errored. The workload manager does not validate method against state
// and consumers read the state out of the payload, so a new terminal state
// needs no new method on the wire.
func (j *jobEvents) finish(state, errMsg string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.terminated {
		return
	}
	j.terminated = true
	completedMs := time.Now().UnixMilli()
	j.wl.CompletedAt = &completedMs
	j.wl.State = state
	if errMsg != "" {
		j.wl.Error = &errMsg
	}
	method := workloadCompletedMethod
	if state != "completed" {
		method = workloadErroredMethod
	}
	j.notifyLocked(method)
}

// notifyLocked numbers the current record and emits it. Caller holds j.mu.
func (j *jobEvents) notifyLocked(method string) {
	j.seq++
	j.wl.Seq = j.seq
	j.emit(method, j.wl)
}
