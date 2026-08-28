// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package applog provides shared structured logging for all nvpair subprocesses.
//
// It wraps log/slog with:
//   - a runtime-mutable LevelVar so the level can be flipped via JSON-RPC,
//   - a text handler on stderr that preserves the existing "[procname] " prefix
//     so raw captures stay readable,
//   - a stdlib "log" bridge so pre-existing log.Printf calls continue to work
//     and also respect the level.
package applog

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	levelVar  = new(slog.LevelVar) // defaults to Info
	initOnce  sync.Once
	procName  string
	handler   slog.Handler
	stderrOut io.Writer = os.Stderr
)

// Init installs the global slog logger and bridges the stdlib "log" package.
// Safe to call multiple times; only the first call takes effect.
func Init(name string, initialLevel slog.Level) {
	initOnce.Do(func() {
		procName = name
		levelVar.Set(initialLevel)

		handler = newPrefixHandler(stderrOut, name, levelVar)
		slog.SetDefault(slog.New(handler))

		// Bridge the stdlib "log" package to slog so pre-existing
		// log.Printf calls go through the same level gate and format.
		// Emits at Info; debug/warn/error callers should use slog directly.
		log.SetOutput(&bridgeWriter{name: name})
		log.SetFlags(0)
		log.SetPrefix("")
	})
}

// SetOutput redirects log output away from os.Stderr. Must be called before
// Init, which builds the handler from this writer; a later call has no effect.
//
// The broker uses this to route its own logs through the same non-blocking sink
// it gives its workers, so a parent that stops reading the stderr pipe cannot
// leave this process blocked in write() while it is trying to exit.
func SetOutput(w io.Writer) {
	stderrOut = w
}

// SetLevel changes the active log level at runtime.
func SetLevel(level slog.Level) {
	levelVar.Set(level)
}

// Level returns the current active log level.
func Level() slog.Level {
	return levelVar.Level()
}

// LevelString returns the current level as a lowercase string
// ("debug"|"info"|"warn"|"error").
func LevelString() string {
	switch levelVar.Level() {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return levelVar.Level().String()
	}
}

// ParseLevel converts a level string to slog.Level.
// Accepts (case-insensitive): debug, info, warn, warning, error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
	}
}

// LevelFromEnv returns the parsed level from NVPAIR_LOG_LEVEL, or the fallback
// if the env var is unset or invalid.
func LevelFromEnv(fallback slog.Level) slog.Level {
	v := os.Getenv("NVPAIR_LOG_LEVEL")
	if v == "" {
		return fallback
	}
	lvl, err := ParseLevel(v)
	if err != nil {
		return fallback
	}
	return lvl
}

// bridgeWriter turns stdlib log.Printf output into slog.Info records so the
// level gate and format apply uniformly.
type bridgeWriter struct {
	name string
}

func (b *bridgeWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	// Drop the old "[name] " prefix if the caller had set one — the handler
	// will re-add it uniformly.
	msg = strings.TrimPrefix(msg, "["+b.name+"] ")
	slog.Info(msg)
	return len(p), nil
}
