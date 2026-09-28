// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var Version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(Version)
		return
	}
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	if err := runHelperCLI(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
}

func runHelperCLI(
	ctx context.Context,
	args []string,
	input io.Reader,
	output io.Writer,
) error {
	if len(args) != 1 {
		return errors.New("expected one fixed helper mode: service, headless-service, serve-console, or request")
	}
	switch args[0] {
	case "service":
		return runPlatformService(ctx)
	case "headless-service":
		return runPlatformHeadlessService(ctx)
	case "serve-console":
		return runHelperServer(ctx, true)
	case "request":
		request, err := readHelperRequest(input)
		if err != nil {
			return err
		}
		response, callErr := callLocalHelper(ctx, request)
		if err := json.NewEncoder(output).Encode(response); err != nil {
			return errors.New("helper response could not be written")
		}
		return callErr
	default:
		return errors.New("unsupported helper mode")
	}
}
