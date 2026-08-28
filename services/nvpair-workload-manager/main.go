// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"nvpair-shared/applog"
	"nvpair-shared/nodeid"
)

// defaultPort is the fixed inter-node HTTP port (spec §7.2) the local events
// server listens on; it's advertised as wl=<port> in this node's _nvpair-node
// record (by the node-scanner daemon) and peers are dialed on the port carried
// in their own directory entry.
const defaultPort = 14320

func main() {
	port := flag.Int("port", defaultPort, "inter-node HTTP port to listen on; the broker registers it as this node's wl service with the discovery daemon")
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	clusterDir := flag.String("cluster-dir", "", "cluster config dir (the .../cluster dir holding node.crt/node.key + trusted/*.json); when set, inter-node traffic uses pinned mTLS scoped to paired cluster members")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-workload-manager", resolveLevel())

	// Resolve this node's stable per-host UUID so the relay self-filter drops
	// our own _nvpair-node record by UUID, not by hostname — otherwise a peer
	// that happens to share our hostname would be wrongly excluded from
	// broadcasts. A node with no stable identity must not run, so fail
	// loudly rather than fall back to the hostname.
	base := ""
	if *clusterDir != "" {
		base = filepath.Dir(*clusterDir)
	}
	selfUUID := nodeid.Resolve(base)
	if selfUUID == "" {
		log.Fatal("could not resolve a stable node UUID (system CSPRNG unavailable); refusing to start")
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
	mgr := NewManager(codec, *port, selfUUID, *clusterDir)

	if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("workload-manager error: %v", err)
	}
	log.Print("shutdown complete")
}
