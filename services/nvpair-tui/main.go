// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nvpair-tui is a terminal UI that spawns and supervises nvpair-ui-broker
// (and, through it, the whole NVPAIR subprocess fleet) over a stdio JSON-RPC
// connection. It is designed to run comfortably over SSH on a headless
// server where the bundled graphical UI cannot run.
//
// Given a subcommand instead of flags it does the opposite: it starts nothing
// and connects to the control socket of an nvpair-tui already running on this
// machine, so pairing can be driven from a script. See cli.go.
//
// This file is the process entrypoint: it parses flags, initialises
// logging, spawns the broker, serves the control socket, and drives the
// supervisor. Logging goes to stderr so it never collides with the
// full-screen TUI on stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"nvpair-shared/applog"
	"nvpair-tui/control"
	"nvpair-tui/pairing"
	"nvpair-tui/rpc"
	"nvpair-tui/ui"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// It mirrors the convention every other component in this repo uses so
// `nvpair-tui --version` reports the value from versions.json.
var Version = "dev"

func main() {
	// A subcommand drives an already-running instance and must not start a
	// broker, a UI, or a second control socket of its own.
	if name := subcommandName(os.Args[1:]); name != "" {
		os.Exit(runSubcommand(newCLIEnv(os.Stdout, os.Stderr), os.Args[1:]))
	}

	brokerPath := flag.String("broker-path", "", "path to nvpair-ui-broker binary (default: ./nvpair-ui-broker alongside this executable)")
	controlSocket := flag.String("control-socket", "", "path to the local control socket to serve (default: the per-user path)")
	noControlSocket := flag.Bool("no-control-socket", false, "do not serve the local control socket (disables the nvpair-tui subcommands against this instance)")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Usage = func() {
		printUsage(flag.CommandLine.Output())
		fmt.Fprintln(flag.CommandLine.Output(), "\nFlags:")
		flag.PrintDefaults()
	}
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

	// One pairing service drives both the Cluster tab and the control socket,
	// so the two can never hold different ideas of what is pending.
	pairs := pairing.NewService(sup.Client)

	// The broker pushes on a single channel with a single consumer, so the
	// fan-out happens here rather than inside the UI: the pairing service has
	// to see cluster:invite-received whether or not the UI is keeping up.
	var brokerReady atomic.Bool
	uiNotifications := make(chan *rpc.Message, notificationBuffer)
	go fanOutNotifications(sup.Client.Notifications(), pairs, &brokerReady, uiNotifications)

	stopControl := serveControlSocket(ctx, *controlSocket, *noControlSocket, pairs, &brokerReady)

	// The broker's stderr (its logs plus every worker's, prefixed) is fed
	// into the UI's Logs view rather than the terminal, so it never
	// collides with the full-screen TUI on stdout.
	if err := ui.Run(ui.Deps{
		Client:        sup.Client,
		Notifications: uiNotifications,
		Stderr:        sup.Stderr,
		Pairing:       pairs,
	}); err != nil {
		slog.Error("ui error", "err", err)
	}

	stopControl()
	sup.Shutdown()
	slog.Info("shutdown complete")
}

// notificationBuffer bounds the queue of broker pushes waiting for the UI. It
// matches the client's own so the fan-out adds no new stall point.
const notificationBuffer = 256

// fanOutNotifications delivers every broker push to the pairing service and
// then to the UI, and records the broker's readiness on the way past.
//
// The pairing service is fed first and synchronously: it must observe an
// invite even if the UI is between frames. The UI's copy is dropped rather
// than blocked on, because a stalled UI must not stall pairing.
//
// The stream closing means the broker is gone, so readiness is withdrawn on
// the way out: a script that asks `ping` after the broker died has to be told
// the truth, or it will go on to send an invite that can only time out.
func fanOutNotifications(in <-chan *rpc.Message, pairs *pairing.Service, ready *atomic.Bool, out chan<- *rpc.Message) {
	defer close(out)
	defer ready.Store(false)
	for msg := range in {
		if msg.Method == "app:ready" {
			ready.Store(true)
		}
		pairs.HandleNotification(msg)
		select {
		case out <- msg:
		default:
			slog.Debug("dropped a broker notification for the UI", "method", msg.Method)
		}
	}
}

// serveControlSocket opens the local control endpoint and starts serving it,
// returning a function that stops it.
//
// A control socket that cannot be opened is a warning, not a failure: the
// interactive UI is nvpair-tui's primary job and it runs without one. The
// warning names the endpoint so an operator whose subcommands fail can see
// why.
func serveControlSocket(ctx context.Context, override string, disabled bool, pairs *pairing.Service, ready *atomic.Bool) func() {
	if disabled {
		slog.Info("control socket disabled by --no-control-socket")
		return func() {}
	}
	path := override
	if path == "" {
		resolved, err := control.DefaultPath()
		if err != nil {
			slog.Warn("no control socket: could not resolve its path", "err", err)
			return func() {}
		}
		path = resolved
	}
	listener, err := control.Listen(path)
	if err != nil {
		slog.Warn("no control socket: the nvpair-tui subcommands will not reach this instance",
			"path", path, "err", err)
		return func() {}
	}
	slog.Info("control socket listening", "path", path)

	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := control.NewServer(pairs, Version, ready.Load).Serve(serveCtx, listener); err != nil {
			slog.Warn("control socket stopped", "err", err)
		}
	}()
	return func() {
		stop()
		_ = listener.Close()
		<-done
	}
}
