// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"io"
	"os"
	"strings"
	"sync"
)

// logOut is where the terminal's own log goes.
//
// The full-screen program owns the terminal while it runs, so a line written to
// stderr then is drawn over by the next frame. While Run is in progress lines
// go to the Logs tab instead, beside the broker's; before and after, they pass
// through to stderr as usual.
var logOut = &logOutput{fallback: os.Stderr}

// LogOutput is the writer to install with applog.SetOutput, before applog.Init.
func LogOutput() io.Writer { return logOut }

type logOutput struct {
	mu       sync.Mutex
	lines    chan<- string
	fallback io.Writer
}

func (o *logOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	lines := o.lines
	o.mu.Unlock()
	if lines == nil {
		return o.fallback.Write(p)
	}
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		// Never blocks. The program's update loop is what drains this channel,
		// and it logs too: a blocking send from there into a full buffer would
		// wait on itself. A line lost to a full buffer is the lesser failure.
		select {
		case lines <- line:
		default:
		}
	}
	return len(p), nil
}

// attach sends lines to the Logs tab until detach.
func (o *logOutput) attach(lines chan<- string) {
	o.mu.Lock()
	o.lines = lines
	o.mu.Unlock()
}

func (o *logOutput) detach() { o.attach(nil) }
