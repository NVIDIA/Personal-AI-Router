// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
)

func (e *Executor) stopLMStudio(ctx context.Context, cli string) error {
	out, err := e.runCommandOutput(ctx, []string{cli, "daemon", "status", "--json"})
	var host struct {
		Status   string `json:"status"`
		IsDaemon *bool  `json:"isDaemon"`
	}
	if err == nil && json.Unmarshal([]byte(out), &host) == nil {
		if host.Status == "not-running" {
			return nil
		}
		if host.Status == "running" && host.IsDaemon != nil && *host.IsDaemon {
			return e.runCommand(ctx, []string{cli, "daemon", "down"})
		}
	}
	return e.runCommand(ctx, []string{cli, "server", "stop"})
}
