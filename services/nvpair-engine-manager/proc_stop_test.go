// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestManagedProcessStopEscalatesWithinBound(t *testing.T) {
	proc, err := startManagedProc(fakeEngineBin, []string{"ignoreterm"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := proc.stop(20 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("bounded stop took %s", elapsed)
	}
	select {
	case <-proc.exited:
	default:
		t.Fatal("process exit was not confirmed")
	}
}
