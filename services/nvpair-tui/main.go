// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nvpair-tui is a terminal UI that spawns and supervises nvpair-ui-broker
// (and, through it, the whole NVPAIR subprocess fleet) over a stdio JSON-RPC
// connection. It is designed to run comfortably over SSH on a headless
// server where the bundled graphical UI cannot run.
//
// This file is the process entrypoint: it parses flags, initialises
// logging, spawns the broker, and drives the supervisor. Logging goes to
// stderr so it never collides with the full-screen TUI on stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
	"nvpair-tui/ui"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// It mirrors the convention every other component in this repo uses so
// `nvpair-tui --version` reports the value from versions.json.
var Version = "dev"

func main() {
	brokerPath := flag.String("broker-path", "", "path to nvpair-ui-broker binary (default: ./nvpair-ui-broker alongside this executable)")
	showVersion := flag.Bool("version", false, "print version and exit")
	appearance := flag.String("appearance", "auto", "terminal background: auto, light, or dark")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	// Before anything renders. Colours are chosen per draw, so a later call
	// would repaint mid-session rather than start correct.
	chosen, ok := ui.ParseAppearance(*appearance)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown --appearance %q: use auto, light, or dark\n", *appearance)
		os.Exit(2)
	}
	ui.SetAppearance(chosen)

	applog.Init("nvpair-tui", resolveLevel())

	resolvedBroker, err := resolveBrokerPath(*brokerPath)
	if err != nil {
		slog.Error("cannot locate broker", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	sup, err := Spawn(ctx, resolvedBroker)
	if err != nil {
		slog.Error("failed to start broker", "err", err)
		os.Exit(1)
	}

	// The broker's stderr (its logs plus every worker's, prefixed) is fed
	// into the UI's Logs view rather than the terminal, so it never
	// collides with the full-screen TUI on stdout.
	outcome, err := ui.Run(sup.Client, sup.Stderr)
	if err != nil {
		slog.Error("ui error", "err", err)
	}

	sup.Shutdown()

	// Only now, with every worker joined, is the data directory unowned. Wiping
	// it while the broker ran would race a shutting-down worker into recreating
	// the files we deleted.
	if outcome.WipeData {
		wipeAppData()
	}

	slog.Info("shutdown complete")
}

// wipeAppData deletes the per-user data directory: node settings, cluster
// identity, trusted peers, and persisted ports. appdir.Dir is the single
// location every component agrees on, so there is one path to remove and no
// guessing at layout.
func wipeAppData() {
	dir, err := appdir.Dir()
	if err != nil {
		slog.Error("cannot resolve the data directory to reset", "err", err)
		return
	}
	// appdir always appends two product segments, so this cannot be a bare home
	// or root directory today. Asserted anyway: this is the one irreversible
	// path in the program, and a relative path would be resolved against
	// whatever directory the process happens to be running in.
	if !filepath.IsAbs(dir) {
		slog.Error("refusing to reset a non-absolute data directory", "dir", dir)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Error("failed to reset data directory", "dir", dir, "err", err)
		return
	}
	slog.Info("data directory reset", "dir", dir)
}
