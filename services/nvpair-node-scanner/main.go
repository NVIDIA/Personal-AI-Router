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
)

func main() {
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	clusterDir := flag.String("cluster-dir", "", "cluster config dir (node.crt/node.key + trusted/); when set, browsed peers holding a pin we trust are annotated trusted in the directory")
	clientCert := flag.String("client-cert", "", "path to PEM client certificate to present when fetching /v1/node-info from TLS-enabled nodes (requires --client-key; node-info enrichment stays plain HTTP if unset)")
	clientKey := flag.String("client-key", "", "path to PEM client private key matching --client-cert")
	caBundle := flag.String("ca-bundle", "", "path to PEM CA bundle to trust for verifying node-info server certificates (additive to the system trust store); enabling any of these TLS flags moves node-info enrichment onto HTTPS")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-node-scanner", resolveLevel())

	// While this node is a cluster member, the daemon advertises its cluster
	// principal as cluster-uuid= and annotates a browsed peer as trusted when it
	// holds a pin for that peer's principal. The mesh is a live view of the
	// cluster dir, so a create/join/leave that happens while this process runs is
	// reflected in what this node advertises (see reloadIdentity).
	mesh := clustertrust.Open(*clusterDir)

	// Optional, dormant-by-default: with none of these flags set, buildTLSClient
	// returns nil and node-info enrichment stays plain HTTP (the standard path).
	// Setting them moves node-info fetches onto HTTPS — scaffolding for a future
	// TLS node-info rollout, gated only on operator flags (never per-node).
	tlsOpts := tlsClientOptions{CertPath: *clientCert, KeyPath: *clientKey, CABundlePath: *caBundle}
	if err := tlsOpts.validate(); err != nil {
		log.Fatalf("invalid TLS configuration: %v", err)
	}
	tlsHTTP, err := buildTLSClient(tlsOpts, nodeInfoFetchTimeout)
	if err != nil {
		log.Fatalf("failed to build node-info TLS client: %v", err)
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
	scanner := NewScanner(codec, mesh, *clusterDir, tlsHTTP)

	if err := scanner.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("scanner error: %v", err)
	}
	log.Print("shutdown complete")
}
