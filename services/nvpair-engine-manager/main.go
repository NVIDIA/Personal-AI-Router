// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
)

// bundledManifests are the default engine manifests compiled into the
// binary, so the service always ships with them regardless of the
// install path. User/third-party manifests are discovered at runtime
// from the per-user config dir and override these by engine name.
//
//go:embed manifests/*.json
var bundledManifests embed.FS

func main() {
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	httpPort := flag.Int("http-port", 0, "if >0, serve the LAN HTTP surface (/v1/models) on this port so peers can enrich this node's model list; 0 disables it")
	controlPort := flag.Int("control-port", 0, "if >0 and this node is clustered, serve the cluster-scoped mTLS remote-control surface (ec: /v1/engines + remote install/pull/start/stop) on this port")
	reservedPort := flag.Int("reserved-port", 0, "local engine port reserved by the parent proxy; 0 disables the reservation")
	clusterDir := flag.String("cluster-dir", "", "cluster identity/pin directory; when set and this node holds a cluster identity, the ec remote-control surface (--control-port) turns on with pin-based mTLS")
	loadedPollSec := flag.Int("loaded-poll-interval", defaultLoadedPollSeconds, "seconds between loaded-model polls that drive engine:models-changed pushes; 0 disables the watcher")
	showVersion := flag.Bool("version", false, "print version and exit")
	uninstallManaged := flag.Bool("uninstall-managed", false, "remove every engine PAIR installed, preserving downloaded models, then exit; for the platform uninstallers")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-engine-manager", resolveLevel())

	// Runs from the platform uninstallers, which have already stopped every
	// engine process and have no broker to talk to, so this path takes no
	// transport and starts no service. It always exits 0: an uninstaller that
	// aborts here would leave the app half-removed.
	if *uninstallManaged {
		runUninstallManaged()
		os.Exit(0)
	}

	var transport io.ReadWriteCloser
	if *ipcPath != "" {
		conn, err := dialIPC(*ipcPath)
		if err != nil {
			log.Fatalf("failed to connect to IPC endpoint %q: %v", *ipcPath, err)
		}
		transport = conn
		log.Printf("using IPC transport: %s", *ipcPath)
	} else {
		transport = newStdioTransport()
		log.Print("using stdio transport")
	}
	defer transport.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("received %s, shutting down", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	codec := NewCodec(transport)
	manifestDir, installBase := userPaths()
	reg := buildRegistry(manifestDir)
	log.Printf("loaded %d engine manifest(s): %v", len(reg.Names()), reg.Names())

	reporter := NewReporter(codec)
	emit := func(method string, params any) { _ = codec.Notify(method, params) }
	exec := NewExecutor(reg, reporter, emit, installBase)
	if err := exec.SetReservedPort(*reservedPort); err != nil {
		log.Fatalf("invalid --reserved-port: %v", err)
	}
	// engine:set-port persists the chosen port as a manifest override in the
	// same per-user engines/ dir that buildRegistry overlays.
	exec.overrideDir = manifestDir
	// Loaded-model watcher cadence. Integer seconds keeps parity with the other
	// flags, so the smallest positive interval is 1s; <=0 disables the watcher.
	if *loadedPollSec <= 0 {
		exec.loadedPollInterval = 0
	} else {
		exec.loadedPollInterval = time.Duration(*loadedPollSec) * time.Second
	}

	// Open a live view of the cluster identity. It powers both directions of
	// remote engine management: serving the ec surface (below) and dialing peers'
	// ec surfaces over pin-based mTLS (the engine:remote-* methods). Keypair
	// presence alone is not membership, so both directions gate on live
	// membership — a left/removed node with a leftover keypair serves and dials
	// nothing, and a node that joins while this process runs starts serving and
	// dialing without a restart.
	mesh := clustertrust.Open(*clusterDir)
	go mesh.Watch(ctx, nil)
	mgr := NewManager(codec, exec, mesh)

	// Optional LAN model-list surface (the em service, for peer discovery
	// enrichment). Off unless the parent opts in with --http-port. A node's model
	// inventory is cluster data, so the LAN side admits only pinned cluster peers;
	// the loopback side stays plain for this node's own scanner, which is what
	// keeps a standalone machine's own model list working. Non-fatal if the port
	// can't bind.
	if *httpPort > 0 {
		go serveHTTP(ctx, *httpPort, exec, mesh)
	}

	// Cluster-scoped mTLS remote-control surface (the ec service). It binds when
	// the parent opts in with --control-port, and admits a caller only while this
	// node is a live cluster member: the TLS identity is resolved per handshake,
	// so an unclustered node presents no leaf and every handshake is refused.
	// Binding on membership instead would leave this surface dark for the life of
	// the process on a node that joins a cluster after engine-manager started —
	// exactly the window in which the broker mints the identity.
	if *controlPort > 0 {
		go serveControl(ctx, *controlPort, exec, mesh)
	}

	if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager error: %v", err)
	}
	log.Print("shutdown complete")
}

// userPaths returns the per-user manifest-override directory and the
// base directory for user-scoped engine installs.
func userPaths() (manifestDir, installBase string) {
	root, err := appdir.Dir()
	if err != nil {
		return "", ""
	}
	return filepath.Join(root, "engines"), filepath.Join(root, "engine-bin")
}

// runUninstallManaged removes the engines PAIR installed, then returns.
//
// The same Executor.UninstallManaged the clients reach over JSON-RPC, so the
// uninstaller gets the identical selection and safety path rather than a second
// implementation of it. There is no broker here and no renderer listening, so
// the reporter and notifier are inert; the work itself is unchanged.
//
// Never fails the caller. A platform uninstaller that aborted here would leave
// the app half-removed, so every outcome is logged and the exit stays clean.
func runUninstallManaged() {
	manifestDir, installBase := userPaths()
	reg := buildBundledRegistry()
	reg.applyLocationOverrides(manifestDir)
	exec := NewExecutor(reg, NewReporter(nil), func(string, any) {}, installBase)
	ctx, cancel := context.WithTimeout(context.Background(), uninstallManagedTimeout)
	defer cancel()
	for _, result := range exec.UninstallManaged(ctx) {
		switch {
		case result.Removed:
			slog.Info("engine removed", "engine", result.Engine)
		case result.Error != "":
			slog.Error("engine could not be removed", "engine", result.Engine, "err", result.Error)
		default:
			slog.Info("engine left in place; PAIR did not install it", "engine", result.Engine)
		}
	}
}

// buildBundledRegistry loads only the manifests compiled into this binary. A
// bad one is a build defect, so it is fatal.
//
// This is the registry the uninstaller path uses. Overriding a manifest means
// choosing commands to run and paths to delete, and the override directory sits
// in the user's own data folder — writable by any process running as them —
// while the Windows uninstaller is elevated (`perMachine`). Overlaying it there
// would let an unprivileged user hand the uninstaller a command to run as
// administrator.
func buildBundledRegistry() *Registry {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		log.Fatalf("bundled manifests invalid: %v", err)
	}
	return reg
}

// buildRegistry loads the embedded manifests then overlays user
// manifests. A bad bundled manifest is a build defect (fatal); a bad
// user manifest is logged and skipped so it can't brick startup.
func buildRegistry(userManifestDir string) *Registry {
	reg := buildBundledRegistry()
	if userManifestDir != "" {
		// Deep-merge per-user overrides onto the bundled manifests (rather
		// than wholesale replace) so a persisted port override pins only
		// runtime.port and keeps inheriting the rest. See registry.go.
		if err := reg.LoadOverrideDir(userManifestDir); err != nil {
			slog.Warn("skipping user manifests after load error", "dir", userManifestDir, "err", err)
		}
	}
	return reg
}
