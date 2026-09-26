// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//go:build windows

package main

import (
	"context"
	"os"

	"github.com/Microsoft/go-winio"
	"golang.org/x/crypto/ssh/agent"
)

func listBootstrapAgentKeys(ctx context.Context) ([]*agent.Key, error) {
	endpoint := os.Getenv("SSH_AUTH_SOCK")
	if endpoint == "" {
		endpoint = `\\.\pipe\openssh-ssh-agent`
	}
	connection, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	return agent.NewClient(connection).List()
}
