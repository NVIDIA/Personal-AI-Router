// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strings"

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

// A new screen or installation gets a new state. An outstanding check
// carries that pointer so it can only change the original row.
type engineUpdateState struct {
	offer                            *engineUpdateOffer
	running, starting, awaitingModel bool
	checking, checkAgain             bool
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
	if !d.enginesQueryable() || d.enginesStale {
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
	if msg.ok {
		entry.offer = msg.offer
		d.refreshEngines()
	}
	return nil
}

func (d *nodeDetail) engineUpdateOffer(engine string) *engineUpdateOffer {
	entry := d.engineUpdates[engine]
	if entry == nil || d.enginesStale || !d.enginesQueryable() {
		return nil
	}
	return entry.offer
}
