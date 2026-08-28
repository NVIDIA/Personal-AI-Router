// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin

package netmon

// netmon_poll.go is the portable fallback backend for platforms without a
// native, event-driven implementation. It does not learn about changes from
// the OS; it just re-checks on a timer. The Monitor only notifies subscribers
// when an enumeration actually differs, so polling is correct (if less prompt
// than a native backend) on any platform.

import "context"

func watchChanges(ctx context.Context, onEvent func()) {
	poll(ctx, onEvent)
}
