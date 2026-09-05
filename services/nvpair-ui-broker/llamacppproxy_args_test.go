// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestLlamaCppProxyArgsHonorPersistedPort(t *testing.T) {
	b := &Broker{}

	got := b.llamacppProxyArgs()
	want := []string{
		"--port",
		"8081",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("llamacppProxyArgs() = %v, want %v", got, want)
	}

	for _, arg := range got {
		if arg == "--ignore-persisted-port" {
			t.Fatal(
				"broker must not suppress the llama.cpp proxy's persisted port",
			)
		}
	}
}

func TestLlamaCppProxyArgsIncludeClusterDir(t *testing.T) {
	b := &Broker{
		clusterDir: "/tmp/pair-cluster",
	}

	got := b.llamacppProxyArgs()
	want := []string{
		"--port",
		"8081",
		"--cluster-dir",
		"/tmp/pair-cluster",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("llamacppProxyArgs() = %v, want %v", got, want)
	}
}
