// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
	"time"

	svcerrors "nvpair-shared/errors"
)

// TestClearIsOfferedOnlyWhereItSticks is the guard for a clear that reported
// success and then undid itself.
//
// Clearing is delete-by-id on the node that receives it; cross-node propagation
// is designed but unbuilt, so clearing a peer's error locally is reverted by the
// next sync from the node that owns it. The broker acknowledges the relay rather
// than the outcome, so the reply is a success either way — which is why this has
// to be decided before the call, not from its result.
func TestClearIsOfferedOnlyWhereItSticks(t *testing.T) {
	const self = "self-uuid"

	mine := svcerrors.ServiceError{ID: "e-local", Message: "boom", NodeID: self}
	theirs := svcerrors.ServiceError{ID: "e-peer", Message: "boom", NodeID: "peer-uuid"}
	unattributed := svcerrors.ServiceError{ID: "e-old", Message: "boom"}

	v := newErrorsView(nil)
	v.SetSize(100, 30)
	v.namer.setSelf(clusterIdentity{NodeUUID: self, Name: "this-host"})
	v.namer.learn("peer-uuid", "peer-host")
	v.setErrors([]svcerrors.ServiceError{mine, theirs, unattributed})

	if !v.clearable(mine) {
		t.Error("own error is not clearable")
	}
	if v.clearable(theirs) {
		t.Error("a peer's error is offered as clearable; the clear would not stick")
	}
	if !v.clearable(unattributed) {
		t.Error("an error with no node id should stay clearable")
	}

	// Selecting the peer's row withdraws the key and explains why, naming the
	// node to go to rather than just refusing.
	v.table.SetCursor(1)
	if got := v.Help(); len(got) != 0 {
		t.Errorf("footer still advertises %d binding(s) on a peer's error", len(got))
	}
	if cmd := v.clearSelected(); cmd != nil {
		t.Error("clearing a peer's error still issued a request")
	}
	if msg := v.status.render(); !strings.Contains(msg, "peer-host") {
		t.Errorf("refusal does not name the node to clear it from: %q", msg)
	}

	// And the local row still works.
	v.table.SetCursor(0)
	if got := v.Help(); len(got) == 0 {
		t.Error("footer withdrew the clear key on this machine's own error")
	}
	if cmd := v.clearSelected(); cmd == nil {
		t.Error("clearing this machine's own error issued no request")
	}
}

// TestErrorContextIsShownForTheSelectedRow is the guard for context the producer
// sends and the screen threw away.
//
// A failure is stamped with the engine, operation, and model it came from, but
// only the message reached the table — so "install failed" arrived with no way
// to tell which engine it meant on a node running two.
func TestErrorContextIsShownForTheSelectedRow(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 20)
	v.setErrors([]svcerrors.ServiceError{{
		ID:         "e1",
		Message:    "install failed",
		Timestamp:  time.Now().UnixMilli(),
		Severity:   "error",
		EngineType: "ollama",
		Operation:  "install",
		ModelName:  "llama3.2",
		Action:     "retry",
	}})

	got := v.View()
	// The display name, not the wire id: the operator sees "Ollama" everywhere
	// else, and an error is a poor place to introduce a second name for it.
	for _, want := range []string{"Ollama", "install", "llama3.2", "retry"} {
		if !contains(got, want) {
			t.Errorf("context %q is missing from the view:\n%s", want, got)
		}
	}
}

// TestErrorContextShowsTheFullMessage checks the detail block carries the whole
// message. The table hard-truncates its MESSAGE cell — about forty characters at
// eighty columns — and this tab exists to show that message, so a long one has
// to be readable somewhere.
func TestErrorContextShowsTheFullMessage(t *testing.T) {
	long := "install failed: could not resolve the download host after three attempts, " +
		"check the machine's network configuration and proxy settings"

	v := newErrorsView(nil)
	v.SetSize(80, 20)
	v.setErrors([]svcerrors.ServiceError{{
		ID: "e1", Message: long, Timestamp: time.Now().UnixMilli(), Severity: "error",
	}})

	got := v.selectedContext()
	// Compared word by word, since the block is wrapped across lines.
	flat := strings.Join(strings.Fields(got), " ")
	if !contains(flat, strings.Join(strings.Fields(long), " ")) {
		t.Errorf("the full message is not in the detail block:\n%s", got)
	}
	// And it must wrap rather than run off the side.
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 80 {
			t.Errorf("detail line is %d columns wide, past the terminal: %q", len(line), line)
		}
	}
}

// TestErrorContextOmitsAbsentFields checks a bare error adds no empty furniture
// beyond its own message.
func TestErrorContextOmitsAbsentFields(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 20)
	v.setErrors([]svcerrors.ServiceError{{
		ID:        "e1",
		Message:   "something went wrong",
		Timestamp: time.Now().UnixMilli(),
		Severity:  "warning",
	}})

	got := v.selectedContext()
	for _, unwanted := range []string{"engine ", "during ", "model ", "suggested"} {
		if contains(got, unwanted) {
			t.Errorf("context invented a %q field: %q", unwanted, got)
		}
	}
}

// TestErrorActionNoneIsNotAdvice checks the producer's way of saying "nothing to
// do" is not rendered as a suggestion.
func TestErrorActionNoneIsNotAdvice(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 20)
	v.setErrors([]svcerrors.ServiceError{{
		ID:         "e1",
		Message:    "informational",
		Timestamp:  time.Now().UnixMilli(),
		EngineType: "lmstudio",
		Action:     "none",
	}})

	got := v.selectedContext()
	if contains(got, "suggested") {
		t.Errorf("action=none was rendered as advice: %q", got)
	}
	if !contains(got, "LM Studio") {
		t.Errorf("the engine was dropped along with it: %q", got)
	}
}

// TestErrorAgeRefreshesOnTick guards the column that used to freeze at whatever
// it read when the error first arrived, making a week-old failure look new.
func TestErrorAgeRefreshesOnTick(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 20)
	v.setErrors([]svcerrors.ServiceError{{
		ID:        "e1",
		Message:   "stuck",
		Timestamp: time.Now().Add(-90 * time.Second).UnixMilli(),
	}})

	v.Update(TickMsg{})
	if got := v.View(); !contains(got, "1m") {
		t.Errorf("age did not refresh on the tick:\n%s", got)
	}
}

var _ View = (*errorsView)(nil)
