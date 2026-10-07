// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"nvpair-tui/rpc"
)

// brokerName is the worker binary the TUI owns. We resolve it next to our
// own executable (the installed bin/ layout puts nvpair-tui beside
// nvpair-ui-broker and all the workers it in turn spawns).
const brokerName = "nvpair-ui-broker"

// shutdownGrace is how long we wait for the broker to exit after asking it to
// shut down (and closing its stdin) before force-killing.
//
// The broker's own teardown is bounded by teardownBudget (everything from the
// start of shutdown through its last worker join) plus
// workloadHistoryFlushJoinTimeout (the final workload-history flush) — both in
// nvpair-ui-broker. This must exceed that sum, or we kill a broker that is still
// legitimately inside its own budget, orphaning whatever workers remain and
// dropping the history flush the budget specifically reserves room for.
//
// Those two currently total 15s, which is also what the desktop parent allows.
// This clock starts after the shutdown request rather than before it, so it needs
// a little headroom on top. If teardownBudget changes, this has to move with it.
const shutdownGrace = 18 * time.Second

// Supervisor owns a single nvpair-ui-broker child process and the JSON-RPC
// client speaking to it over stdio. The broker, in turn, spawns and
// supervises the rest of the NVPAIR fleet, so owning the broker means owning
// everything.
type Supervisor struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	Client *rpc.Client
	Stderr io.ReadCloser
}

// resolveBrokerPath honours an explicit override, otherwise looks for the
// broker binary alongside this executable. We deliberately do not fall
// back to PATH: the TUI ships in the same bin/ dir as the broker, and a
// stray PATH match for a stale dev build is worse than a clear error.
func resolveBrokerPath(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("broker binary %q not accessible: %w", override, err)
		}
		return override, nil
	}
	bin := brokerName
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate own executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), bin)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("%s not found next to nvpair-tui (use --broker-path to override): %w", bin, err)
	}
	return candidate, nil
}

// Spawn launches the broker and starts the JSON-RPC read loop. The child
// runs with its working directory set to the broker's own directory so
// the broker's sibling-binary worker resolution finds nvpair-node-scanner et
// al. ctx governs the client read loop; use Shutdown for an orderly stop.
func Spawn(ctx context.Context, brokerPath string) (*Supervisor, error) {
	cmd := exec.Command(brokerPath)
	cmd.Dir = filepath.Dir(brokerPath)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("broker stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start broker: %w", err)
	}

	client := rpc.NewClient(stdout, stdin)
	go client.Run(ctx)

	return &Supervisor{cmd: cmd, stdin: stdin, Client: client, Stderr: stderr}, nil
}

// Shutdown asks the broker to stop cleanly: send the shutdown RPC, close
// its stdin (a second, EOF-based stop signal), then wait up to
// shutdownGrace before killing it. The broker tears its own workers down
// in response, so this leaves no orphans.
func (s *Supervisor) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = s.Client.Call(ctx, "shutdown", nil)
	cancel()

	_ = s.stdin.Close()

	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownGrace):
		_ = s.cmd.Process.Kill()
		<-done
	}
}
