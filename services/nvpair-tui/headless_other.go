// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"
	"errors"
	"net"
)

func headlessUnsupported() error {
	return errors.New("private headless onboarding control is currently supported only on Linux; the interactive TUI remains available")
}
func listenHeadless() (net.Listener, func(), error)  { return nil, nil, headlessUnsupported() }
func dialHeadless(context.Context) (net.Conn, error) { return nil, headlessUnsupported() }
func headlessSameUser(net.Conn) bool                 { return false }
func headlessPortConflicts() error                   { return headlessUnsupported() }
func runHeadlessService(context.Context, string, string, string) (any, error) {
	return nil, headlessUnsupported()
}
func runHeadlessUpgrade(context.Context, headlessUpgradeRequest, string, string) (any, error) {
	return nil, headlessUnsupported()
}
