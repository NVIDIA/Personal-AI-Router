// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

const maxStartupOutputBytes = 8 * 1024

var metalMemoryMessage = regexp.MustCompile(`\[metal\][^\r\n]*insufficient memory`)

// startupOutput retains a bounded, redacted diagnostic tail and remembers
// recognized Metal memory failures. It is discarded when startup succeeds.
type startupOutput struct {
	mu                 sync.Mutex
	tail               string
	pending            string
	private            *regexp.Regexp
	lookback           int
	closed             bool
	metalMemoryFailure bool
}

func (o *startupOutput) Write(p []byte) (int, error) {
	n := len(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	for !o.closed && len(p) > 0 {
		size := min(len(p), maxStartupOutputBytes)
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
	// Classify only redacted output. Remember the reason before bounding the
	// tail: a long follow-up traceback can otherwise displace the root failure.
	if !o.metalMemoryFailure {
		diagnostic := strings.ToLower(o.tail + text)
		o.metalMemoryFailure = strings.Contains(diagnostic, "kiogpucommandbuffercallbackerroroutofmemory") ||
			metalMemoryMessage.MatchString(diagnostic)
	}
	if len(text) > maxStartupOutputBytes {
		text = text[len(text)-maxStartupOutputBytes:]
	}
	o.tail += text
	if len(o.tail) > maxStartupOutputBytes {
		o.tail = o.tail[len(o.tail)-maxStartupOutputBytes:]
	}
}

func (o *startupOutput) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.tail = ""
	o.pending = ""
	o.metalMemoryFailure = false
}

// Keep the original cause available to errors.Is without presenting generic
// launch-options advice as the explanation for a recognized memory failure.
type startupMemoryError struct{ cause error }

func (e *startupMemoryError) Error() string {
	return "Not enough available GPU memory to start this engine. Unload models from other engines or free memory, then try again."
}

func (e *startupMemoryError) Unwrap() error { return e.cause }

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
	slices.SortFunc(values, func(a, b string) int {
		return cmp.Compare(len(b), len(a))
	})
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

func (o *startupOutput) failure(ctx context.Context, err error) error {
	o.mu.Lock()
	o.flush(true)
	text := o.tail
	memoryFailure := o.metalMemoryFailure
	o.mu.Unlock()
	// CommandContext may return an ExitError when it kills the child. Preserve
	// that cause, but use the operation context to identify cancellation/timeouts.
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	memoryFailure = memoryFailure && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	if memoryFailure {
		err = &startupMemoryError{cause: err}
	}
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "")))
	if text == "" {
		return err
	}
	if memoryFailure {
		return fmt.Errorf("%w\n\nEngine details:\n%s", err, text)
	}
	return fmt.Errorf("%w: %s", err, text)
}

func launchDiagnosticArgs(rt Runtime) []string {
	return rt.LaunchArgs
}
