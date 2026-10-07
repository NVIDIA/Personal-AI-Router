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
	"syscall"

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
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

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
	if err := ui.Run(sup.Client, sup.Stderr); err != nil {
		slog.Error("ui error", "err", err)
	}

	sup.Shutdown()
	slog.Info("shutdown complete")
}
