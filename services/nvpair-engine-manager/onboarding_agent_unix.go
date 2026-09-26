// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package main

import (
	"context"
	"errors"
	"net"
	"os"

	"golang.org/x/crypto/ssh/agent"
)

func listBootstrapAgentKeys(ctx context.Context) ([]*agent.Key, error) {
	endpoint := os.Getenv("SSH_AUTH_SOCK")
	if endpoint == "" {
		return nil, errors.New("SSH agent is unavailable")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	return agent.NewClient(connection).List()
}
