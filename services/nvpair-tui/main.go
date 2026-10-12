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
	"path/filepath"
	"sync/atomic"
	"syscall"

	"nvpair-shared/appdir"
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
	appearance := flag.String("appearance", "auto", "terminal background: auto, light, or dark")
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

	// Started before the program, not merely before the first draw. On auto
	// this asks the terminal for its background and reads the answer, which
	// only works while stdin is still ours — once Bubble Tea is running, its
	// reader takes the reply and the query learns nothing. Joined below, after
	// the broker is up, so a terminal that never answers costs its timeout
	// alongside startup rather than in front of it.
	chosen, ok := ui.ParseAppearance(*appearance)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown --appearance %q: use auto, light, or dark\n", *appearance)
		os.Exit(2)
	}
	appearanceSettled := ui.StartAppearance(chosen)

	// Before Init, which builds its handler from the output it finds. While the
	// full-screen program runs this sends the log to the Logs tab, where it can
	// be read; stderr is hidden behind the program then.
	applog.SetOutput(ui.LogOutput())
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

	// One pairing service drives both the Nodes tab and the control socket,
	// so the two can never hold different ideas of what is pending.
	pairs := pairing.NewService(sup.Client)

	// The broker pushes on a single channel with a single consumer, so the
	// fan-out happens here rather than inside the UI: the pairing service has
	// to see cluster:invite-received whether or not the UI is keeping up.
	var brokerReady atomic.Bool
	uiNotifications := make(chan *rpc.Message, notificationBuffer)
	go fanOutNotifications(sup.Client.Notifications(), pairs, &brokerReady, uiNotifications)

	stopControl := serveControlSocket(ctx, *controlSocket, *noControlSocket, pairs, &brokerReady)

	// The last moment the answer can be had: Bubble Tea takes stdin next, and
	// the first frame is already choosing colours with it.
	appearanceSettled()
	// Recorded because a wrong answer is visible but unexplained: the operator
	// sees colours that do not suit their terminal and has nothing telling
	// them what PAIR concluded, or that --appearance would override it.
	slog.Debug("terminal appearance", "requested", string(chosen),
		"using", string(ui.DetectedAppearance()))

	// The broker's stderr (its logs plus every worker's, prefixed) is fed
	// into the UI's Logs view rather than the terminal, so it never
	// collides with the full-screen TUI on stdout.
	outcome, err := ui.Run(ui.Deps{
		Client:        sup.Client,
		Notifications: uiNotifications,
		Stderr:        sup.Stderr,
		Pairing:       pairs,
	})
	if err != nil {
		slog.Error("ui error", "err", err)
	}

	stopControl()
	sup.Shutdown()

	// Only now, with every worker joined, is the data directory unowned. Wiping
	// it while the broker ran would race a shutting-down worker into recreating
	// the files we deleted.
	if outcome.WipeData {
		wipeAppData()
	}

	slog.Info("shutdown complete")
}

// notificationBuffer bounds the queue of broker pushes waiting for the UI. It
// matches the client's own so the fan-out adds no new stall point.
const notificationBuffer = 256

// fanOutNotifications delivers every broker push to the pairing service and
// then to the UI, and records the broker's readiness on the way past.
//
// The pairing service is fed first and synchronously: it must observe an
// invite even if the UI is between frames. The UI's copy goes through
// relayInOrder, so a UI that falls behind neither stalls pairing nor loses a
// push — a lost workload update would leave a job showing as running forever.
//
// The stream closing means the broker is gone, so readiness is withdrawn on
// the way out: a script that asks `ping` after the broker died has to be told
// the truth, or it will go on to send an invite that can only time out.
func fanOutNotifications(in <-chan *rpc.Message, pairs *pairing.Service, ready *atomic.Bool, out chan<- *rpc.Message) {
	toUI := make(chan *rpc.Message)
	go relayInOrder(toUI, out)
	defer close(toUI)
	defer ready.Store(false)
	for msg := range in {
		if msg.Method == "app:ready" {
			ready.Store(true)
		}
		pairs.HandleNotification(msg)
		toUI <- msg
	}
}

// relayInOrder forwards everything from in to out in order, holding what out
// is not ready for, and closes out once in is closed and the backlog drained.
// It is always ready to receive, so a sender never waits on out's reader.
func relayInOrder(in <-chan *rpc.Message, out chan<- *rpc.Message) {
	defer close(out)
	var backlog []*rpc.Message
	for in != nil || len(backlog) > 0 {
		var send chan<- *rpc.Message
		var next *rpc.Message
		if len(backlog) > 0 {
			send, next = out, backlog[0]
		}
		select {
		case msg, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			backlog = append(backlog, msg)
		case send <- next:
			backlog[0] = nil
			backlog = backlog[1:]
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
