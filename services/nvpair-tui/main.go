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
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	headless := flag.Bool("headless", false, "run the Linux backend parent with private same-account onboarding control")
	control := flag.Bool("control", false, "exchange one private onboarding request using non-terminal stdin/stdout")
	service := flag.String("headless-service", "", "manage the fixed Linux user service: install, start, stop, status, uninstall, upgrade")
	lifetime := flag.String("startup-lifetime", "persistent", "headless service lifetime: persistent (requires existing lingering), or explicit session-only mode")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}
	modes := 0
	for _, enabled := range []bool{*headless, *control, *service != ""} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "choose exactly one headless mode")
		os.Exit(2)
	}
	if *control {
		if err := privateControlPipes(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		deadline := time.Now().Add(headlessRequestTimeout)
		_ = os.Stdin.SetReadDeadline(deadline)
		_ = os.Stdout.SetWriteDeadline(deadline)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if err := runHeadlessControl(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *service != "" {
		ctx, cancel := context.WithTimeout(context.Background(), headlessServiceTimeout(*service))
		defer cancel()
		var result any
		var err error
		if *service == "upgrade" {
			if err = privateControlPipes(); err == nil {
				inputCtx, inputCancel := context.WithTimeout(ctx, 10*time.Second)
				request, readErr := readHeadlessUpgrade(inputCtx, os.Stdin)
				inputCancel()
				if readErr != nil {
					err = readErr
				} else {
					result, err = runHeadlessUpgrade(ctx, request, *brokerPath, *lifetime)
				}
			}
		} else {
			result, err = runHeadlessService(ctx, *service, *brokerPath, *lifetime)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return
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
	if *headless {
		if err := runHeadless(ctx, resolvedBroker); err != nil {
			slog.Error("headless parent stopped", "err", err)
			os.Exit(1)
		}
		return
	}

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
