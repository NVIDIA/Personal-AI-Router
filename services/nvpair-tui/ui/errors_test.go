// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"
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

	assert.True(t, v.clearable(mine), "own error is not clearable")
	assert.False(t, v.clearable(theirs), "a peer's error must not be offered as clearable; the clear would not stick")
	assert.True(t, v.clearable(unattributed), "an error with no node id should stay clearable")

	// Selecting the peer's row withdraws the key and explains why, naming the
	// node to go to rather than just refusing.
	v.table.SetCursor(1)
	assert.Empty(t, v.Help(), "footer must withdraw bindings on a peer's error")
	assert.Nil(t, v.clearSelected(), "clearing a peer's error must not issue a request")
	assert.Contains(t, v.status.render(), "peer-host", "refusal must name the node to clear it from")

	// And the local row still works.
	v.table.SetCursor(0)
	assert.NotEmpty(t, v.Help(), "footer withdrew the clear key on this machine's own error")
	assert.NotNil(t, v.clearSelected(), "clearing this machine's own error issued no request")
}

// TestNothingIsClearedBeforeThisMachineIsKnown checks the clear waits for the
// identity it depends on. Before it arrives there is no telling this machine's
// errors from a peer's, so offering the key meant guessing.
func TestNothingIsClearedBeforeThisMachineIsKnown(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 30)
	v.setErrors([]svcerrors.ServiceError{{ID: "e", Message: "boom", NodeID: "some-uuid"}})

	assert.False(t, v.clearable(v.errs[0]), "an error was clearable before this machine's identity was known")
	assert.Nil(t, v.clearSelected(), "a clear must wait for this machine's identity")
	assert.Contains(t, v.status.render(), "identifying", "the refusal must say why")

	v.namer.setSelf(clusterIdentity{NodeUUID: "some-uuid"})
	assert.NotNil(t, v.clearSelected(), "this machine's own error could not be cleared once it was known")
}

// TestInitialReadDoesNotOverwriteANewerPush is the regression guard for an
// errors:update lost at startup. The initial read's reply and the pushes arrive
// by different paths, and a reply landing after a push replaced the newer
// snapshot with an older one until the next change.
func TestInitialReadDoesNotOverwriteANewerPush(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(100, 30)
	params := []byte(`[{"id":"new","message":"fresh","severity":"error"}]`)
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "errors:update", Params: params}})
	v.Update(errorsLoadedMsg{errs: []svcerrors.ServiceError{{ID: "old", Message: "stale"}}})

	require.Len(t, v.errs, 1, "the pushed snapshot must be kept")
	assert.Equal(t, "new", v.errs[0].ID, "the pushed snapshot must be kept")
}

// TestErrorColumnsFitTheNarrowestTerminal checks the table's minimums fit the
// forty-column floor. They summed past it, and what was clipped off the right
// was the message — the one column this tab exists to show.
func TestErrorColumnsFitTheNarrowestTerminal(t *testing.T) {
	v := newErrorsView(nil)
	v.SetSize(minTerminalWidth, 20)
	total := 0
	var message int
	for _, c := range v.columns() {
		total += c.Width + cellPadding
		if c.Title == "MESSAGE" {
			message = c.Width
		}
	}
	assert.LessOrEqual(t, total, minTerminalWidth, "columns must fit the terminal")
	assert.GreaterOrEqual(t, message, 10, "MESSAGE must remain readable at the minimum width")
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
	assert.Contains(t, got, "Ollama", "context must be shown in the view")
	assert.Contains(t, got, "install", "context must be shown in the view")
	assert.Contains(t, got, "llama3.2", "context must be shown in the view")
	assert.Contains(t, got, "retry", "context must be shown in the view")
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
	assert.Contains(t, flat, strings.Join(strings.Fields(long), " "), "the full message must be in the detail block")
	// And it must wrap rather than run off the side.
	for _, line := range strings.Split(got, "\n") {
		assert.LessOrEqual(t, len(line), 80, "detail line must fit the terminal: %q", line)
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
	assert.NotContains(t, got, "engine ", "context must omit absent fields")
	assert.NotContains(t, got, "during ", "context must omit absent fields")
	assert.NotContains(t, got, "model ", "context must omit absent fields")
	assert.NotContains(t, got, "suggested", "context must omit absent fields")
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
	assert.NotContains(t, got, "suggested", "action=none must not be rendered as advice")
	assert.Contains(t, got, "LM Studio", "the engine must not be dropped along with it")
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
	assert.Contains(t, v.View(), "1m", "age must refresh on the tick")
}

var _ View = (*errorsView)(nil)
