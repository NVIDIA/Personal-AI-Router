// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const maxStartupOutput = 8 * 1024

// startupOutput retains a bounded diagnostic tail without interpreting vendor
// messages or option names. It is discarded when startup succeeds.
type startupOutput struct {
	mu       sync.Mutex
	tail     string
	pending  string
	private  *regexp.Regexp
	lookback int
	closed   bool
}

func (o *startupOutput) Write(p []byte) (int, error) {
	n := len(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	for !o.closed && len(p) > 0 {
		size := min(len(p), maxStartupOutput)
		o.pending += string(p[:size])
		p = p[size:]
		o.flush(false)
	}
	return n, nil
}

// flush redacts before bounding the tail. Keep enough undecided input to match
// the longest private value across Write calls, independent of output volume.
// The caller holds mu.
func (o *startupOutput) flush(final bool) {
	end := len(o.pending)
	if !final {
		end -= o.lookback
	}
	if end <= 0 {
		return
	}
	start := 0
	if o.private != nil {
		for _, match := range o.private.FindAllStringIndex(o.pending, -1) {
			if match[0] >= end {
				break
			}
			o.appendTail(o.pending[start:match[0]])
			o.appendTail("[redacted]")
			start = match[1]
			end = max(end, start)
		}
	}
	o.appendTail(o.pending[start:end])
	o.pending = o.pending[end:]
}

func (o *startupOutput) appendTail(text string) {
	if len(text) > maxStartupOutput {
		text = text[len(text)-maxStartupOutput:]
	}
	o.tail += text
	if len(o.tail) > maxStartupOutput {
		o.tail = o.tail[len(o.tail)-maxStartupOutput:]
	}
}

func (o *startupOutput) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.tail = ""
	o.pending = ""
}

func newStartupOutput(args []string, env map[string]string) *startupOutput {
	// Redact supplied values generically, while preserving flag names such as
	// --unknown-option so the engine's explanation remains useful.
	values := make([]string, 0, len(args)+len(env))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if _, value, ok := strings.Cut(arg, "="); ok {
				values = append(values, value)
			}
		} else {
			values = append(values, arg)
		}
	}
	for _, value := range env {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	o := &startupOutput{}
	patterns := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			patterns = append(patterns, regexp.QuoteMeta(value))
			o.lookback = max(o.lookback, len(value)-1)
		}
	}
	if len(patterns) > 0 {
		o.private = regexp.MustCompile(strings.Join(patterns, "|"))
	}
	return o
}

func (o *startupOutput) failure(err error) error {
	o.mu.Lock()
	o.flush(true)
	text := o.tail
	o.mu.Unlock()
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "")))
	if text == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, text)
}

func launchDiagnosticArgs(rt Runtime) []string {
	if rt.LaunchArgs != nil {
		return *rt.LaunchArgs
	}
	return nil
}
