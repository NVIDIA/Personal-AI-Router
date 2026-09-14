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

	"nvpair-shared/appdir"
	"nvpair-shared/applog"
	"nvpair-shared/parentwatch"
)

// defaultSettingsPath returns the canonical on-disk location for
// settings.json. Kept alongside manual-nodes.json
// so all NVPAIR config for the current user is co-located.
func defaultSettingsPath() string {
	if p, err := appdir.Path("settings.json"); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "settings.json")
	}
	return "settings.json"
}

func main() {
	ipcPath := flag.String("ipc", "", "IPC endpoint: Unix domain socket path or Windows named pipe (default: stdin/stdout)")
	showVersion := flag.Bool("version", false, "print version and exit")
	settingsPath := flag.String("settings", "", "path to settings.json (default: the per-user Nvidia Corporation/Personal AI Router data dir)")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-node-settings", resolveLevel())

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

	path := *settingsPath
	if path == "" {
		path = defaultSettingsPath()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stdin EOF is this process's usual "my parent is gone" signal, but it is
	// only delivered once EVERY holder of the pipe closes it -- and an Electron
	// helper that outlives the app inherits that descriptor. Watching the parent
	// directly is what actually guarantees no orphan is left holding a port.
	defer parentwatch.Start("nvpair-node-settings", cancel)()

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
	mgr, err := NewManager(codec, path)
	if err != nil {
		log.Fatalf("failed to initialise settings manager: %v", err)
	}

	if err := mgr.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("manager error: %v", err)
	}
	log.Print("shutdown complete")
}
