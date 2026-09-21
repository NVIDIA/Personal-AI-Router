// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	settings "nvpair-shared/enginesettings"
)

// handleSettingsPortRPC serves the port-only RPCs — engine:set-port,
// proxy:set-port, and lmstudio-proxy:set-port — which nvpair-tui calls to move
// a single port without rendering the full launch settings form. They keep
// their own narrow request and response shapes, but run through the same
// authoritative settings operation as the desktop editor, so a port change
// made from the terminal cannot diverge from one made from the UI. In
// particular a proxy change now fails on a busy port instead of silently
// binding a different one.
//
// proxyEngine selects which port the request addresses: "" is the engine's own
// server port, and a non-empty value names the proxy in front of that engine.
func (b *Broker) handleSettingsPortRPC(msg *Message, proxyEngine string) {
	var p struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	if json.Unmarshal(msg.Params, &p) != nil || p.Port < 1 || p.Port > 65535 {
		_ = b.codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535")
		return
	}
	if proxyEngine != "" {
		p.Engine = proxyEngine
	}
	if b.rejectOllamaHostAliasPort(msg, p.Port, p.Engine) {
		return
	}
	target := settings.Request{Engine: p.Engine}
	snapshot, err := b.getEngineSettings(context.Background(), target, "")
	if err == nil {
		target.Settings = snapshot.Settings
		target.ExpectedRevision = snapshot.Revision
		target.RequestID = settingsID()
		target.Resolution = "server"
		if proxyEngine != "" {
			target.Settings.ProxyPort = p.Port
		} else {
			target.Settings.ServerPort = p.Port
		}
		var receipt settingsReceipt
		receipt, err = b.applyEngineSettings(context.Background(), target, "")
		if err == nil && receipt.Phase == "failed" {
			err = fmt.Errorf("settings were saved but could not be applied; inspect engine settings for the current state")
		}
	}
	if err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	if proxyEngine != "" {
		_ = b.codec.Respond(msg.ID, map[string]int{"port": p.Port})
		return
	}
	var status json.RawMessage
	if err = b.settingsWorkerCall(context.Background(), "engine:status", map[string]string{"engine": p.Engine}, &status); err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	_ = b.codec.Respond(msg.ID, status)
}
