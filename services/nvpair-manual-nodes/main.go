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
	clientCert := flag.String("client-cert", "", "path to PEM client certificate to present when probing TLS-enabled manual nodes")
	clientKey := flag.String("client-key", "", "path to PEM client private key matching --client-cert")
	caBundle := flag.String("ca-bundle", "", "path to PEM bundle of CAs to trust for verifying server certificates (additive to system trust store)")
	clusterDir := flag.String("cluster-dir", "", "cluster config dir (node.crt/node.key + trusted/); when set, a TLS manual node is probed over cluster mTLS with our pinned leaf")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-manual-nodes", resolveLevel())

	// While this node is a cluster member, a TLS manual node is probed over
	// cluster mTLS (the only way to reach a clustered peer's node-info);
	// otherwise the BYO --client-cert / plaintext paths are unchanged. The mesh is
	// a live view of the cluster dir, so probes start using the cluster leaf as
	// soon as this node joins, without a restart.
	mesh := clustertrust.Open(*clusterDir)

	tlsOpts := tlsClientOptions{
		CertPath:     *clientCert,
		KeyPath:      *clientKey,
		CABundlePath: *caBundle,
	}
	if err := tlsOpts.validate(); err != nil {
		log.Fatalf("invalid TLS configuration: %v", err)
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
	mgr, err := NewManager(codec, tlsOpts, mesh)
	if err != nil {
		log.Fatalf("failed to construct manager: %v", err)
	}

	if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager error: %v", err)
	}
	log.Print("shutdown complete")
}
