// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin

package main

// procImage has no implementation on hosts outside the three supported
// platforms. Windows queries the process image directly, Linux reads
// /proc/<pid>/exe, and macOS asks lsof for the txt descriptor; none of those
// mechanisms is portable to an arbitrary Unix, and guessing one would mean
// trusting whatever binary a PATH lookup happened to find to decide which
// process PAIR is allowed to kill.
//
// Returning "" makes the ownership check in isOurEngineImage fail closed, so
// stop and uninstall decline an adopted engine here rather than terminating a
// process they cannot identify. That is a real limitation, not a stub with
// nothing behind it: orphan reclaim does not work on these hosts. It is stated
// plainly because the same silent "" on macOS is the bug this file's siblings
// exist to fix.
func procImage(int) string { return "" }
