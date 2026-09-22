// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for cancelling one in-flight request by id.
//
// The registry this drives is per facade rather than per process, and the
// reason is only visible with more than one facade enabled: each facade mints
// request ids from its own counter starting at 1, so "cancel request 1" is
// ambiguous across engines and the runId guard cannot disambiguate it —
// runId names the process, which every facade shares.

import (
	"context"
	"encoding/json"
	"testing"

	"nvpair-shared/engines"
)

// cancelResult drives workload/cancel through the control plane and returns
// what the caller was told.
func cancelResult(t *testing.T, p *Proxy, rec *recordingWriter, engine, id, runID string) bool {
	t.Helper()
	before := len(rec.lines())
	params, err := json.Marshal(map[string]string{"id": id, "runId": runID})
	if err != nil {
		t.Fatalf("marshal cancel params: %v", err)
	}
	msgID := json.RawMessage(`1`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod(engine, "workload/cancel"),
		Params: params,
		ID:     &msgID,
	})

	lines := rec.lines()
	if len(lines) <= before {
		t.Fatalf("workload/cancel for %s/%s produced no response", engine, id)
	}
	var reply struct {
		Result struct {
			Accepted bool `json:"accepted"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &reply); err != nil {
		t.Fatalf("decode cancel reply: %v", err)
	}
	if reply.Error != nil {
		t.Fatalf("workload/cancel for %s/%s errored: %s", engine, id, reply.Error.Message)
	}
	return reply.Result.Accepted
}

// twoFacadeRecordingProxy is twoFacadeProxy with its upward frames captured, so
// a test can read the reply to a request rather than only observe side effects.
func twoFacadeRecordingProxy(t *testing.T) (*Proxy, *recordingWriter) {
	t.Helper()
	redirectConfigDir(t)

	rec := &recordingWriter{}
	p := NewProxy(NewCodec(rec))
	p.serveCtx = t.Context()
	for _, e := range engines.All() {
		port := freeTCPPort(t)
		if _, err := p.enableFacade(enableFacadeParams{
			Engine:              e.Name,
			Port:                port,
			IgnorePersistedPort: true,
		}); err != nil {
			t.Fatalf("enable %s facade on :%d: %v", e.Name, port, err)
		}
	}
	t.Cleanup(func() { p.shutdown(t.Context()) })
	return p, rec
}

// A cancel names one request of one engine. Accepting it must cancel exactly
// that request's context and nothing else.
func TestCancelAbortsOnlyTheNamedRequest(t *testing.T) {
	p, rec := twoFacadeRecordingProxy(t)
	f := p.facadeFor(engines.All()[0].Name)

	first, cancelFirst := context.WithCancel(context.Background())
	second, cancelSecond := context.WithCancel(context.Background())
	_, forgetFirst := f.trackInflight("1", cancelFirst)
	_, forgetSecond := f.trackInflight("2", cancelSecond)
	defer forgetFirst()
	defer forgetSecond()

	if cancelResult(t, p, rec, f.profile.Name, "missing", p.runID) {
		t.Error("a request id that was never registered was accepted")
	}
	if first.Err() != nil || second.Err() != nil {
		t.Fatal("an unmatched cancel aborted a live request")
	}

	if !cancelResult(t, p, rec, f.profile.Name, "1", p.runID) {
		t.Fatal("the named request was not accepted")
	}
	if first.Err() != context.Canceled {
		t.Errorf("named request context = %v, want cancelled", first.Err())
	}
	if second.Err() != nil {
		t.Errorf("unrelated request was cancelled: %v", second.Err())
	}
}

// runId names the proxy process, and request ids restart with it. A cancel
// carrying a previous run's id refers to a request that no longer exists, and
// the id it names may now belong to an unrelated one.
func TestCancelRefusesAStaleRun(t *testing.T) {
	p, rec := twoFacadeRecordingProxy(t)
	f := p.facadeFor(engines.All()[0].Name)

	ctx, cancel := context.WithCancel(context.Background())
	_, forget := f.trackInflight("1", cancel)
	defer forget()

	if cancelResult(t, p, rec, f.profile.Name, "1", p.runID+"-previous") {
		t.Error("a cancel from a previous run was accepted")
	}
	if ctx.Err() != nil {
		t.Errorf("a stale run cancelled a live request: %v", ctx.Err())
	}
}

// The property a process-wide registry would break. Both facades have a
// request numbered "1", because each counts from 1; a cancel addressed to one
// engine must leave the other engine's request of the same number running.
func TestCancelIsScopedToTheAddressedFacade(t *testing.T) {
	p, rec := twoFacadeRecordingProxy(t)
	all := engines.All()
	target := p.facadeFor(all[0].Name)
	bystander := p.facadeFor(all[1].Name)

	targetCtx, cancelTarget := context.WithCancel(context.Background())
	bystanderCtx, cancelBystander := context.WithCancel(context.Background())
	_, forgetTarget := target.trackInflight("1", cancelTarget)
	_, forgetBystander := bystander.trackInflight("1", cancelBystander)
	defer forgetTarget()
	defer forgetBystander()

	if !cancelResult(t, p, rec, target.profile.Name, "1", p.runID) {
		t.Fatalf("%s request 1 was not accepted", target.profile.Name)
	}
	if targetCtx.Err() != context.Canceled {
		t.Errorf("%s request 1 = %v, want cancelled", target.profile.Name, targetCtx.Err())
	}
	if bystanderCtx.Err() != nil {
		t.Fatalf("cancelling %s request 1 also aborted %s request 1",
			target.profile.Name, bystander.profile.Name)
	}
}

// An asked-for cancel and a client hanging up both surface as a cancelled
// request context. The workload's error text is user-visible, so the two must
// not be reported the same way.
func TestCancelIsDistinguishedFromAClientDisconnect(t *testing.T) {
	p, _ := twoFacadeRecordingProxy(t)
	f := p.facadeFor(engines.All()[0].Name)

	cancelled, cancelOne := context.WithCancel(context.Background())
	defer cancelOne()
	req, forget := f.trackInflight("1", cancelOne)
	defer forget()
	if req.cancelled.Load() {
		t.Fatal("a freshly tracked request already reports being cancelled")
	}
	if !f.cancelInflight("1") {
		t.Fatal("cancel was not accepted")
	}
	if !req.cancelled.Load() {
		t.Error("an asked-for cancel was not distinguished from a disconnect")
	}
	if cancelled.Err() != context.Canceled {
		t.Errorf("request context = %v, want cancelled", cancelled.Err())
	}
}

// Forgetting a finished request keeps a later cancel of the same id from
// reaching a request that has since reused it.
func TestCancelFindsNothingAfterTheRequestFinishes(t *testing.T) {
	p, rec := twoFacadeRecordingProxy(t)
	f := p.facadeFor(engines.All()[0].Name)

	_, cancel := context.WithCancel(context.Background())
	_, forget := f.trackInflight("1", cancel)
	forget()

	if cancelResult(t, p, rec, f.profile.Name, "1", p.runID) {
		t.Error("a finished request was still cancellable")
	}
}
