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
	"syscall"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/engines"
)

func main() {
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	clusterDir := flag.String("cluster-dir", "", "cluster trust directory (node.crt/key + trusted pins); enables the LAN mTLS inference ingress when this node is clustered")
	clusterRouter := flag.Bool("cluster-router", false, "start the cluster routing endpoint (OpenAI-compatible, all engines) on loopback; the desktop turns this on from its Cluster Settings switch")
	clusterRouterPort := flag.Int("cluster-router-port", routerDefaultPort, "port for the cluster routing endpoint when --cluster-router is set; loopback-only regardless")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	// The process starts with no engine facade and no listener. Which engines
	// it fronts, on which ports, and with which alias addresses all arrive over
	// facade/enable, because the broker computes a different port plan per
	// engine and a flag cannot express more than one of them.
	//
	// The log component is therefore the process, not an engine: there is no
	// profile to name here. Facade-scoped records carry an engine field so a
	// line is still attributable once several facades share this sink.
	applog.Init(engines.ProxyComponent, resolveLevel())

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
	proxy := NewProxy(codec)
	// Open a live view of this node's cluster mTLS trust fabric. While unclustered
	// the proxy serves only the loopback plaintext personality; once this node is
	// a member the same listener also serves the pin-gated LAN mTLS ingress, and
	// peers become routable candidates. The proxy needs no restart to notice
	// either transition — it re-derives membership per request and on a watch.
	//
	// Membership is gated on an active admission or a pin, never on keypair
	// presence: a left/removed node keeps its keypair by design, and would
	// otherwise keep logging cluster_ingress with no cluster peers to serve.
	proxy.mesh = clustertrust.Open(*clusterDir)

	// The cluster router is a second routing personality hosted by this same
	// process: it shares the transports, the trust fabric, and the workload
	// stream, and adds a loopback endpoint that routes across every enabled
	// engine. It stays dark unless the flag asks for it — a machine that only
	// needs per-engine endpoints binds no extra port. The desktop asks for it
	// from its Cluster Settings switch, which is why the port is a number a
	// person can change rather than a fixed one.
	if *clusterRouter {
		// Nil means "this process has no cluster endpoint", whether the port
		// was taken or none was asked for: router/status reports no endpoint,
		// and a pushed alias document stays on the process — bringUpRouter
		// installs it into whatever endpoint comes up later, so a late
		// endpoint is not nameless. Everything else here keeps serving, and
		// the same states are reachable later at runtime through
		// router/set-enabled; see bringUpRouter.
		router, err := proxy.bringUpRouter(ctx, *clusterRouterPort)
		if err != nil {
			slog.Warn("cluster routing endpoint is down; the engine facades are unaffected",
				"port", *clusterRouterPort, "err", err)
		} else {
			proxy.clusterRouter = router
		}
	}

	go proxy.mesh.Watch(ctx, func(clustered bool) {
		slog.Info("cluster inference ingress switched personality", "cluster_ingress", clustered)
		proxy.dropUnpinnedPeerTransports()
	})

	if err := proxy.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("proxy error: %v", err)
	}
	log.Print("shutdown complete")
}
