// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux

package main

// ssPID has no implementation outside Linux. ss belongs to iproute2 and does not
// exist on macOS in any location.
//
// Declared rather than left to a shared PATH lookup on purpose. Resolving a tool
// that is never installed can only find something a PATH entry supplied, and the
// PID this returns is the one the caller signals — so on macOS the bare-name
// fallback was a way for a user-writable directory to nominate a process for
// termination, reachable whenever the lsof lookup ahead of it failed for an
// unrelated reason.
//
// lsof is the only port-owner mechanism here, and failing to resolve one
// declines the stop.
func ssPID(int) (int, bool) { return 0, false }
