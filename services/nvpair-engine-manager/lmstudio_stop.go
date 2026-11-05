// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

func (e *Executor) stopLMStudio(ctx context.Context, cli string) error {
	out, err := e.runCommandOutput(ctx, []string{cli, "daemon", "status", "--json"})
	if err != nil {
		return fmt.Errorf("identify LM Studio host: %w", err)
	}
	var host struct {
		Status   string `json:"status"`
		IsDaemon *bool  `json:"isDaemon"`
	}
	if err := json.Unmarshal([]byte(out), &host); err != nil {
		return fmt.Errorf("decode LM Studio host status: %w", err)
	}
	if host.Status == "not-running" {
		return nil
	}
	if host.Status != "running" || host.IsDaemon == nil {
		return fmt.Errorf("LM Studio host status does not identify a running GUI or daemon")
	}
	if *host.IsDaemon {
		return e.runCommand(ctx, []string{cli, "daemon", "down"})
	}
	return e.runCommand(ctx, []string{cli, "server", "stop"})
}
