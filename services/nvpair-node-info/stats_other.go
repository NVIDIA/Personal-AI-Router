// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin

package main

// Stubs for hosts without a platform collector. Windows uses PDH, Linux uses
// /proc + nvidia-smi, and macOS uses Mach-backed gopsutil readers.
//
// Static CPU / memory data (name, core count, total bytes) is still
// reported on these platforms via ghw in cpu_detect.go / memory_detect.go —
// those calls don't go through this collector; only the dynamic
// per-tick numbers do, and those are legitimately unknown here.

func luidKey(low uint32, high int32) string { return "" }

// statsCollector on non-Windows is a pure no-op. Snapshot() returns
// a zero-valued statsSnapshot so every dynamic field drops from JSON
// via the per-field omitempty tags: missing means "unknown" rather
// than a literal zero.
type statsCollector struct{}

func startStatsCollector() *statsCollector { return &statsCollector{} }

func (c *statsCollector) Snapshot() statsSnapshot { return statsSnapshot{} }

func (c *statsCollector) Stop() {}
