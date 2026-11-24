// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"
)

const lmsInstallAttempts = 3

func (e *Executor) installWithRetry(ctx context.Context, engine string, st *engineState) error {
	if engine != "lmstudio" {
		return e.installAttempt(ctx, engine, st)
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := e.installAttempt(ctx, engine, st)
		if err == nil {
			return nil
		}
		// A stopped caller must not be mistaken for a retryable network timeout.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		kind := classifyLMSInstallFailure(err)
		if kind == "" {
			return err
		}
		slog.Warn("LM Studio installer download failed", "engine", engine,
			"attempt", attempt, "of", lmsInstallAttempts, "category", kind, "err", err)
		if attempt == lmsInstallAttempts {
			return &lmsInstallFailure{cause: err, kind: kind, attempts: attempt}
		}
		// installAttempt has already removed its temporary downloads. Keep
		// progress in flight until a new attempt starts or the operation ends.
		timer := time.NewTimer(e.installRetryBackoff * time.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
