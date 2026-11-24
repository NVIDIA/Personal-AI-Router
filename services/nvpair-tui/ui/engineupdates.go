// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"nvpair-shared/engines"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

type engineUpdateOffer struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	InstallType    string `json:"installType"`
	DownloadURL    string `json:"downloadUrl"`
}

// A new screen or installation gets a new state. An outstanding check or
// apply reply carries that pointer so it can only change the original row.
type engineUpdateState struct {
	offer                            *engineUpdateOffer
	running, starting, awaitingModel bool
	checking, checkAgain             bool
	apply                            *engineUpdateOperation
}

// The terminal progress frame and the RPC reply can arrive in either order.
type engineUpdateOperation struct {
	terminal string
	detached bool
}

// The node's inventory can arrive every five seconds. Ask the vendor when
// installation or lifecycle state changes and on the six-hour refresh.
func (d *nodeDetail) syncEngineUpdates(periodic bool) tea.Cmd {
	if d.enginesStale || !d.enginesQueryable() || strings.TrimSpace(os.Getenv(disableUpdateCheckEnv)) != "" {
		return nil
	}
	for id := range d.engineUpdates {
		if !slices.ContainsFunc(d.engines, func(e engineStatus) bool { return e.Engine == id && e.Installed }) {
			delete(d.engineUpdates, id)
		}
	}
	var cmds []tea.Cmd
	for _, e := range d.engines {
		if _, supported := engines.ByName(e.Engine); !supported || !e.Installed {
			continue
		}
		entry := d.engineUpdates[e.Engine]
		changed := entry == nil || entry.running != e.Running || entry.starting != e.Starting || entry.awaitingModel != e.AwaitingModel
		if entry == nil {
			entry = &engineUpdateState{}
			d.engineUpdates[e.Engine] = entry
		}
		entry.running, entry.starting, entry.awaitingModel = e.Running, e.Starting, e.AwaitingModel
		if !e.Starting && (changed || periodic) {
			cmds = append(cmds, d.checkEngineUpdate(e.Engine, entry))
		}
	}
	return tea.Batch(cmds...)
}

type engineUpdateCheckedMsg struct {
	engine string
	entry  *engineUpdateState
	offer  *engineUpdateOffer
	ok     bool
}

func (d *nodeDetail) checkEngineUpdate(engine string, entry *engineUpdateState) tea.Cmd {
	if entry.apply != nil || !d.enginesQueryable() || d.enginesStale {
		return nil
	}
	if entry.checking {
		entry.checkAgain = true
		return nil
	}
	entry.checking = true
	return d.own(callEngine(d.client, d.nodeArg(), engine, "engine:check-updates", callTimeout, func(reply *rpc.Message, err error) tea.Msg {
		msg := engineUpdateCheckedMsg{engine: engine, entry: entry}
		var result struct {
			Offer json.RawMessage `json:"updateAvailable"`
		}
		if err == nil && json.Unmarshal(reply.Result, &result) == nil && json.Unmarshal(result.Offer, &msg.offer) == nil {
			msg.ok = msg.offer == nil || msg.offer.valid()
		}
		return msg
	}))
}

func (offer *engineUpdateOffer) valid() bool {
	// These values will be written to a terminal, including on a peer's behalf.
	printable := func(s string, limit int) bool {
		return s != "" && len(s) <= limit && strings.IndexFunc(s, func(r rune) bool { return r < '!' || r > '~' }) < 0
	}
	if !printable(offer.CurrentVersion, 128) || !printable(offer.LatestVersion, 128) {
		return false
	}
	if offer.InstallType == "managed" {
		return true
	}
	if offer.InstallType != "external" || !printable(offer.DownloadURL, 2048) {
		return false
	}
	u, err := url.Parse(offer.DownloadURL)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func (d *nodeDetail) receiveEngineUpdateCheck(msg engineUpdateCheckedMsg) tea.Cmd {
	entry := msg.entry
	if d.engineUpdates[msg.engine] != entry {
		return nil
	}
	entry.checking = false
	if entry.checkAgain {
		entry.checkAgain = false
		return d.checkEngineUpdate(msg.engine, entry)
	}
	// A failed or unavailable check leaves the last confirmed offer on screen.
	if msg.ok && entry.apply == nil {
		entry.offer = msg.offer
		d.refreshEngines()
	}
	return nil
}

func (d *nodeDetail) engineUpdateOffer(engine string) *engineUpdateOffer {
	entry := d.engineUpdates[engine]
	if entry == nil || entry.apply != nil || d.enginesStale || !d.enginesQueryable() {
		return nil
	}
	return entry.offer
}

type engineUpdateAppliedMsg struct {
	engineOpMsg
	entry *engineUpdateState
}

func (d *nodeDetail) applyEngineUpdate(e *engineStatus) tea.Cmd {
	if e == nil || !e.Installed {
		return nil
	}
	offer := d.engineUpdateOffer(e.Engine)
	if offer == nil || offer.InstallType != "managed" {
		return nil
	}
	entry := d.engineUpdates[e.Engine]
	entry.apply = &engineUpdateOperation{}
	d.refreshEngines()
	d.status.busy("updating %s on %s...", e.label(), d.node.name)
	// Match the desktop's remote transfer budget for a full engine download.
	const updateBudget = 6 * time.Hour
	engine := e.Engine
	op := engineOps["update"]
	return d.own(callEngine(d.client, d.nodeArg(), engine, op.method, updateBudget, func(_ *rpc.Message, err error) tea.Msg {
		return engineUpdateAppliedMsg{classifyOpResult(op.what, engine, "update", err), entry}
	}))
}

func (d *nodeDetail) finishEngineUpdate(msg engineUpdateAppliedMsg) tea.Cmd {
	entry := d.engineUpdates[msg.engine]
	if entry != msg.entry || entry.apply == nil {
		return nil
	}
	if msg.detached && entry.apply.terminal == "" {
		entry.apply.detached = true
		if !d.promptArmed() {
			d.status.info("update %s on %s is continuing in the background", d.engineLabel(msg.engine), d.node.name)
		}
		return nil
	}
	switch {
	case msg.detached:
		// The terminal frame already reported the outcome.
		if entry.apply.terminal == "done" {
			entry.offer = nil
		}
	case msg.err != nil:
		if !d.promptArmed() {
			d.status.error("update %s on %s failed: %s", d.engineLabel(msg.engine), d.node.name, msg.err)
		}
	default:
		entry.offer = nil
		if !d.promptArmed() {
			d.status.ok("updated %s on %s", d.engineLabel(msg.engine), d.node.name)
		}
	}
	entry.apply = nil
	d.refreshEngines()
	return d.checkEngineUpdate(msg.engine, entry)
}

func (d *nodeDetail) showEngineUpdateProgress(engine, stage string, percent *int, failure string) tea.Cmd {
	if !d.promptArmed() {
		switch stage {
		case "done":
			d.status.ok("updated %s on %s", d.engineLabel(engine), d.node.name)
		case "failed":
			d.status.error("update %s on %s failed: %s", d.engineLabel(engine), d.node.name, failure)
		default:
			d.status.busy("update %s on %s: %s%s", d.engineLabel(engine), d.node.name, stage, progressPercent(percent))
		}
	}
	// Another console may have started this update. Our own update reply
	// schedules the check even if its last progress frame never reaches us.
	entry := d.engineUpdates[engine]
	if entry == nil || (stage != "done" && stage != "failed") {
		return nil
	}
	if entry.apply != nil {
		entry.apply.terminal = stage
		if entry.apply.detached {
			return d.finishEngineUpdate(engineUpdateAppliedMsg{engineOpMsg{engine: engine, detached: true}, entry})
		}
	} else if stage == "done" && entry.offer != nil {
		entry.offer = nil
		d.refreshEngines()
		return d.checkEngineUpdate(engine, entry)
	}
	return nil
}
