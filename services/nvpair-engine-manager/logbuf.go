// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"
)

// maxEngineLogLines bounds each engine's captured-output ring.
const maxEngineLogLines = 2000

// LogLine is one captured stdout/stderr line from a managed engine.
type LogLine struct {
	Time   string `json:"time"`
	Stream string `json:"stream"` // "stdout" | "stderr"
	Text   string `json:"text"`
}

// logBuffer is a per-engine bounded ring of recent output lines.
type logBuffer struct {
	mu    sync.Mutex
	lines []LogLine
}

func newLogBuffer() *logBuffer {
	return &logBuffer{lines: make([]LogLine, 0, 256)}
}

func (b *logBuffer) append(stream, text string) {
	line := LogLine{Time: time.Now().Format("15:04:05.000"), Stream: stream, Text: text}
	b.mu.Lock()
	if len(b.lines) >= maxEngineLogLines {
		// Copy the surviving half into a fresh slice so the dropped
		// portion's backing array can be reclaimed.
		keep := b.lines[len(b.lines)-maxEngineLogLines/2:]
		b.lines = append(make([]LogLine, 0, maxEngineLogLines), keep...)
	}
	b.lines = append(b.lines, line)
	b.mu.Unlock()
}

func (b *logBuffer) snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogLine, len(b.lines))
	copy(out, b.lines)
	return out
}
