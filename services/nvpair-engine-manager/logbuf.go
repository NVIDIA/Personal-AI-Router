// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
	"time"

	"nvpair-shared/enginelogs"
)

// maxEngineLogLines bounds each engine's captured-output ring.
const maxEngineLogLines = enginelogs.MaxLines

// LogLine is one captured stdout/stderr line from a managed engine.
type LogLine struct {
	Time   string `json:"time"`
	Stream string `json:"stream"` // "stdout" | "stderr"
	Text   string `json:"text"`
}

// logBuffer is a per-engine bounded ring of recent output lines.
type logBuffer struct {
	mu        sync.Mutex
	lines     []LogLine
	textBytes int
}

func newLogBuffer() *logBuffer {
	return &logBuffer{lines: make([]LogLine, 0, 256)}
}

func (b *logBuffer) append(stream, text string) {
	if len(text) > enginelogs.MaxLineBytes || len(text) > enginelogs.MaxSnapshotTextBytes {
		return
	}
	line := LogLine{Time: time.Now().Format("15:04:05.000"), Stream: stream, Text: text}

	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.lines) > 0 && (len(b.lines) >= maxEngineLogLines || b.textBytes+len(text) > enginelogs.MaxSnapshotTextBytes) {
		b.textBytes -= len(b.lines[0].Text)
		b.lines[0] = LogLine{}
		b.lines = b.lines[1:]
	}
	b.lines = append(b.lines, line)
	b.textBytes += len(text)
}

func (b *logBuffer) snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LogLine, len(b.lines))
	copy(out, b.lines)
	return out
}
