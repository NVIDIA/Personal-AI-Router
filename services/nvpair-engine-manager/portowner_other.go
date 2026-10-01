// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux

package main

// ssPID has no implementation outside Linux. ss belongs to iproute2 and does not
// exist on macOS in any location.
//
// It returns nothing rather than resolving a bare name through PATH: a tool that
// is never installed can only be found where a PATH entry supplied it, and the
// PID it returned would be the one the caller signals. lsof is the only
// port-owner mechanism here, and failing to resolve one declines the stop.
func ssPID(int) (int, bool) { return 0, false }
