// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/rand"
	"encoding/hex"

	"nvpair-shared/enginesettings"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// Engine launch settings: the arguments and environment an engine is started
// with, edited here and owned by nvpair-engine-manager.
//
// The wire types are the backend's own (nvpair-shared/enginesettings) rather
// than a local copy. A second declaration of a revision-carrying contract is a
// second thing to keep in step, and getting the revision wrong is not a
// compile error — it is a lost update.
//
// Three calls, in a fixed order, because the backend validates and normalizes
// before it commits:
//
//  1. engine:get-settings returns the snapshot, including the revision every
//     later write must carry and whether the engine is editable at all.
//  2. engine:preview-settings validates a draft without saving it. It answers
//     with normalized settings, per-field errors, a port conflict, and whether
//     applying would restart the engine.
//  3. engine:apply-settings commits the *normalized* settings the preview
//     returned, not the raw draft, so what is saved is what was validated.
//
// The broker routes all three to a peer when the request names one, so this is
// the same path for this machine and for a node across the cluster.

// newSettingsRequestID mints the identifier a commit is required to carry.
//
// It is an idempotency key, not a trace id. The backend records a receipt
// against it, so replaying the same id with the same settings returns the
// original outcome instead of applying twice — and replaying it with different
// settings is refused outright. That makes it wrong to reuse one across edits
// and wrong to send none at all, which is what an apply without it was: the
// backend rejected it with "a request identifier is required" after the
// preview had already passed.
//
// Random rather than a counter, because the backend keys receipts per engine
// across every client and a restarted terminal would begin counting again.
// Sixteen bytes of hex is 32 characters, well inside the 128 the broker allows.
func newSettingsRequestID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// crypto/rand does not fail in practice, and an empty id would be
		// rejected by the broker rather than silently losing idempotency.
		return ""
	}
	return hex.EncodeToString(id[:])
}

// settingsResolution tells the backend which side wins when the server-port
// field and the port inside the command text disagree.
//
// They are two views of one number, and a user editing either one expects
// theirs to stick. Sending no resolution is not a third option: the backend
// reports the disagreement as a conflict rather than guessing, which is the
// right default for an API and the wrong experience in an editor.
const (
	// resolutionServer: the numeric field was edited, so rewrite the command.
	resolutionServer = "server"
	// resolutionLaunch: the command text was edited, so update the field.
	resolutionLaunch = "launch"
)

// engineSettingsMsg carries a fetched or pushed snapshot.
type engineSettingsMsg struct {
	snapshot enginesettings.Snapshot
	err      error
}

// enginePreviewMsg carries a validated draft, still uncommitted.
//
// It holds the request that produced it so the apply step can reuse the
// revision and target without rebuilding them from view state that may have
// moved on.
type enginePreviewMsg struct {
	request enginesettings.Request
	preview enginesettings.Preview
	err     error
}

// engineSettingsAppliedMsg is the outcome of a commit.
type engineSettingsAppliedMsg struct {
	engine string
	err    error
}

// getEngineSettingsCmd fetches one engine's settings snapshot. An empty nodeID
// means this machine.
func getEngineSettingsCmd(client *rpc.Client, nodeID, engine string) tea.Cmd {
	return call(client, "engine:get-settings",
		enginesettings.Request{NodeID: nodeID, Engine: engine},
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return engineSettingsMsg{err: err}
			}
			var snap enginesettings.Snapshot
			if derr := decodeParams(msg.Result, &snap); derr != nil {
				return engineSettingsMsg{err: derr}
			}
			return engineSettingsMsg{snapshot: snap}
		})
}

// previewEngineSettingsCmd validates a draft without saving it.
func previewEngineSettingsCmd(client *rpc.Client, req enginesettings.Request) tea.Cmd {
	return call(client, "engine:preview-settings", req,
		func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return enginePreviewMsg{request: req, err: err}
			}
			var preview enginesettings.Preview
			if derr := decodeParams(msg.Result, &preview); derr != nil {
				return enginePreviewMsg{request: req, err: derr}
			}
			return enginePreviewMsg{request: req, preview: preview}
		})
}

// applyEngineSettingsCmd commits settings the backend has already normalized.
//
// The caller is expected to have settled the draft through a preview first —
// see judgeSettingsPreview, which both substitutes the normalized settings and
// drops the resolution. This does not re-check either, because a commit that
// quietly repaired its own request would hide the bug that produced it.
func applyEngineSettingsCmd(client *rpc.Client, req enginesettings.Request) tea.Cmd {
	return call(client, "engine:apply-settings", req,
		func(_ *rpc.Message, err error) tea.Msg {
			return engineSettingsAppliedMsg{engine: req.Engine, err: err}
		})
}

// settingsUnavailableReason explains why an engine cannot be configured, or is
// empty when it can.
//
// The backend decides this and says why; repeating its rules here would be a
// second opinion that drifts. The only judgement made locally is to supply
// wording when it reports a bare "not editable".
func settingsUnavailableReason(snap enginesettings.Snapshot) string {
	if snap.Editable {
		return ""
	}
	if snap.Reason != "" {
		return snap.Reason
	}
	if snap.Adopted {
		return "this engine was already running when PAIR found it, so PAIR does not own how it starts"
	}
	return "this engine's startup settings cannot be edited right now"
}
