// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Version is set by build scripts with -X main.Version=...
var Version = "dev"

func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "Configuration error: %v\n", err)
		return 2
	}
	if cfg.Version {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	client, err := newBackendClient(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "Configuration error: %v\n", err)
		return 2
	}
	if cfg.ListModels {
		models, err := client.listModels(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "Model query failed: %v\n", err)
			return 2
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(models); err != nil {
			fmt.Fprintf(stderr, "Output error: %v\n", err)
			return 2
		}
		return 0
	}
	return newDispatcher(cfg, client, stdout, stderr).run(ctx)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runMain(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
