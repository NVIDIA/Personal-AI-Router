// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"
)

func TestFixedRankWorkerTransport(t *testing.T) {
	argv, body, err := fixedVLLMSystemRankCommand("start", vllmRankSystemPlan{}, nil)
	if err != nil || len(argv) != 5 || argv[0] != "/usr/bin/python3" || argv[1] != "-I" || argv[2] != "-S" || !strings.Contains(string(body), `"action":"start"`) {
		t.Fatal("fixed interpreter/request binding failed", err)
	}
	if _, _, err := fixedVLLMSystemRankCommand("execute", vllmRankSystemPlan{}, nil); err == nil {
		t.Fatal("generic action admitted")
	}
	if _, _, err := fixedVLLMSystemRankCommand("normal-stop", vllmRankSystemPlan{}, nil); err == nil {
		t.Fatal("unbound normal stop admitted")
	}
	if _, body, err := fixedVLLMSystemRankCommand("normal-status", vllmRankSystemPlan{}, &vllmRankSystemIdentity{}); err != nil || !strings.Contains(string(body), `"action":"normal-status"`) {
		t.Fatal("bound normal status was unavailable")
	}
	if _, _, err := fixedVLLMSystemRankCommand("start", vllmRankSystemPlan{}, &vllmRankSystemIdentity{}); err == nil {
		t.Fatal("start accepted stop-only identity")
	}
}

// Linux caps a single exec argument at 128 KiB, and the worker ships inline.
func TestFixedRankWorkerFitsOneExecArgument(t *testing.T) {
	argv, input, err := vllmSystemRankInvocation("start", vllmRankSystemPlan{NodeID: "node-a"}, nil, &diagnosticPackageElevation{NodeID: "node-a", NonInteractive: true})
	defer clear(input)
	if err != nil || len(argv) == 0 || len(argv[len(argv)-1]) >= 128<<10 {
		t.Fatalf("the inline fixed rank worker no longer fits one exec argument: %v", err)
	}
}

func TestFixedRankWorkerNormalizesBoundedSystemdArrayReadback(t *testing.T) {
	for _, fragment := range []string{
		`repeatable = {"DeviceAllow", "IPAddressAllow", "IPAddressDeny"}`,
		`if not line.strip():`,
		`if key in repeatable:`,
		`result[key] = " ".join(part for part in (result[key], value) if part)`,
		`"StandardOutput=journal", "StandardError=journal"`,
		`"StandardOutput": "journal", "StandardError": "journal"`,
		`stdout=None, stderr=None`,
	} {
		if !strings.Contains(vllmRankSystemWorkerSource, fragment) {
			t.Fatalf("fixed rank worker lost bounded systemd array normalization: %s", fragment)
		}
	}
}
