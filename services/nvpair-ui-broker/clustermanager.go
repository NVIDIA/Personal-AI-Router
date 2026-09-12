// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// clusterManagerCallTimeout bounds a relayed cluster:* / nodes:* request. It is
// generous because cluster:invite-node and cluster:respond-to-invite drive
// multi-round-trip inter-node pairing exchanges over the network before
// answering.
const clusterManagerCallTimeout = 30 * time.Second

// clusterManagerProcess is the broker's handle on its child
// nvpair-cluster-manager. Like ollama-proxy it is a full bidirectional JSON-RPC
// peer: it answers id-bearing cluster:* / nodes:* requests and pushes
// cluster:invite-received / cluster:invite-declined / cluster:identity-changed /
// nodes:changed (and any other cluster:* notifications) through to the client.
// notifications the broker forwards to clients. The id-correlation + read pump
// live in the shared jsonrpc.Peer; this handle owns the OS process lifecycle.
type clusterManagerProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	peer     *Peer
	done     chan struct{}
	onNotify func(method string, params json.RawMessage)

	// relayDir is the broker's LAN directory. When the cluster-manager sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots down for its invite/member address resolution. nil
	// when the relay isn't wired.
	relayDir *relay.Directory
	subMu    sync.Mutex
	subID    int
}

// SetLogLevel forwards an already-validated log level to the cluster-manager as
// a log/set-level notification (through the peer's codec, so it can't interleave
// with an in-flight Call).
func (c *clusterManagerProcess) SetLogLevel(level string) error {
	return c.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the
// cluster-manager process has exited (cmd.Wait returned).
func (c *clusterManagerProcess) Done() <-chan struct{} { return c.done }

// startClusterManager spawns nvpair-cluster-manager with the broker's current log
// level, hides the console window on Windows, and runs the peer read pump.
// onNotify (may be nil) is invoked for every notification it emits so the
// broker can forward its event stream to clients.
//
// configDir is the base directory whose cluster/ subtree the manager owns. The
// broker passes its OWN resolved base so both sides address one directory: the
// manager is the only writer of the cluster dir and the workers' only source of
// membership, so if it resolved a different base than the broker handed the
// workers, the node would pair successfully into a directory nothing reads —
// healthy roster, no cluster traffic. An empty configDir leaves the manager on its
// own default (standalone invocation).
func startClusterManager(binaryPath, logLevel, configDir string, relayDir *relay.Directory, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*clusterManagerProcess, error) {
	args := append([]string{"--log-level", logLevel}, extraArgs...)
	if configDir != "" {
		args = append(args, "--config-dir", configDir)
	}
	cmd := exec.Command(binaryPath, args...)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = stderrOut

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}

	cp := &clusterManagerProcess{
		cmd:      cmd,
		stdin:    stdin,
		peer:     NewPeer(NewCodec(readWriter{stdout, stdin})),
		done:     make(chan struct{}),
		onNotify: onNotify,
		relayDir: relayDir,
	}

	go cp.peer.Serve(nil, cp.handleNotify)
	go func() {
		_ = cmd.Wait()
		cp.peer.Close() // wake any in-flight Call; make fresh Calls fail fast
		cp.subMu.Lock()
		if cp.subID != 0 && cp.relayDir != nil {
			cp.relayDir.Unsubscribe(cp.subID)
			cp.subID = 0
		}
		cp.subMu.Unlock()
		close(cp.done)
	}()

	return cp, nil
}

func (c *clusterManagerProcess) handleNotify(method string, params json.RawMessage) {
	// The cluster-manager subscribes upward for its peer resolver: wire
	// it to the relay directory and push events down, rather than forwarding this
	// as a client-facing event.
	if method == noderec.MethodSubscribe {
		c.handleSubscribe(params)
		return
	}
	if c.onNotify != nil {
		c.onNotify(method, params)
	}
}

// handleSubscribe wires the cluster-manager's discovery:subscribe into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the peer, then sends the initial snapshot. A re-subscribe drops
// the prior registration first so it isn't double-fed.
func (c *clusterManagerProcess) handleSubscribe(params json.RawMessage) {
	if c.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		if err := c.peer.Notify(noderec.NotifyNodes, noderec.GetNodesResult{Nodes: nodes}); err != nil {
			slog.Debug("failed to push node snapshot to cluster-manager", "err", err)
		}
	}
	c.subMu.Lock()
	if c.subID != 0 {
		c.relayDir.Unsubscribe(c.subID)
	}
	id, sub, err := subscribeRelay(c.relayDir, params, send)
	c.subID = id
	c.subMu.Unlock()
	if err != nil {
		slog.Warn("cluster-manager sent invalid discovery:subscribe", "err", err)
		return
	}
	c.relayDir.Deliver(sub)
}

// Call issues an id-bearing request to the cluster-manager and blocks until the
// matching response arrives, the context is cancelled, the process exits, or
// clusterManagerCallTimeout elapses.
func (c *clusterManagerProcess) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	ctx, cancel := context.WithTimeout(ctx, clusterManagerCallTimeout)
	defer cancel()
	return c.peer.Call(ctx, method, params)
}

// Stop signals the cluster-manager to exit by closing its stdin (observed as
// EOF) and waits for it to exit, escalating if it does not — see
// waitForStdinClose. Safe to call multiple times.
func (c *clusterManagerProcess) Stop() {
	waitForStdinClose("cluster-manager", c.cmd, c.stdin, c.done)
}
