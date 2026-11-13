// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"

	"nvpair-shared/noderec"
)

// hardwareSummary renders the broker's snapshot directly. Valid GPU utilization
// includes its sample age; validity alone does not mean a reading is recent.
func hardwareSummary(t noderec.NodeInfo) []string {
	lines := make([]string, 0, len(t.GPUs)+2)
	for _, g := range t.GPUs {
		lines = append(lines, "  "+gpuLine(g, t.TelemetryValid, t.MSSince))
	}
	if t.CPU != nil {
		name := t.CPU.Name
		if name == "" {
			name = "CPU"
		}
		detail := fmt.Sprintf("  %-28s %3d%%", truncate(name, 28), t.CPU.UtilizationPercent)
		if t.CPU.Cores > 0 {
			detail += fmt.Sprintf("  %d cores", t.CPU.Cores)
		}
		lines = append(lines, detail)
	}
	if t.Memory != nil && t.Memory.TotalBytes > 0 {
		lines = append(lines, fmt.Sprintf("  %-28s %s / %s", "RAM",
			humanBytes(t.Memory.UsedBytes), humanBytes(t.Memory.TotalBytes)))
	}
	return lines
}

// gpuLine renders one GPU. Utilization is omitted when the node reports its
// dynamic sample as unusable, rather than printing a zero that reads as idle.
func gpuLine(g noderec.GPUInfo, valid bool, msSince int64) string {
	name := g.Name
	if name == "" {
		name = "GPU"
	}
	util := "  --%"
	if valid {
		util = fmt.Sprintf("%3d%% (%s ago)", g.UtilizationPercent, hardwareSampleAge(msSince))
	}
	line := fmt.Sprintf("%-28s %s", truncate(name, 28), util)
	if g.VramBytes > 0 {
		line += fmt.Sprintf("  %s / %s", humanBytes(g.VramUsedBytes), humanBytes(g.VramBytes))
	}
	return line
}

func hardwareSampleAge(msSince int64) string {
	seconds := max(msSince, 0) / 1000
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 60*60:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 24*60*60:
		return fmt.Sprintf("%dh", seconds/(60*60))
	default:
		return fmt.Sprintf("%dd", seconds/(24*60*60))
	}
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	value := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[idx])
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}
