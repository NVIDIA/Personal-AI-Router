// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestHeadlessServiceLaunchesOnlyFixedBroker(t *testing.T) {
	command := windowsHeadlessBrokerCommand()
	if command.Path != windowsHeadlessBrokerPath ||
		!reflect.DeepEqual(command.Args, []string{windowsHeadlessBrokerPath}) ||
		command.Dir != filepath.Dir(windowsHeadlessBrokerPath) {
		t.Fatalf("headless broker command = %#v", command.Args)
	}
	if windowsHeadlessShutdownGrace < 18*time.Second {
		t.Fatalf("shutdown grace = %s", windowsHeadlessShutdownGrace)
	}
}
