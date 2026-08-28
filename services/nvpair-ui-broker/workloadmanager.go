// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// workloadManagerProcess is the broker's handle on its child
// nvpair-workload-manager. Unlike node-info (fire-and-forget) it
// is bidirectional, but notification-only: the broker writes local-origin
// workload:* / workloads:remove frames to its stdin (for cluster
// broadcast) and reads workloads:upsert / workloads:remove frames back off
// its stdout (peer-origin events to relay to clients). There is no
// id-bearing request/response layer on this interface (spec 7.1).
//
// The handle owns the OS process, the stdin pipe used to signal clean
// shutdown (via EOF) and to forward frames (workload events, log/set-level),
// and a done channel that closes after cmd.Wait returns.
type workloadManagerProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdinMu serializes Forward / log-level writes against the Close() in
	// Stop(); see nodeInfoProcess.stdinMu.
	stdinMu sync.Mutex
	done    chan struct{}

	// onNotify, if non-nil, is invoked on the reader goroutine for every
	// notification the manager emits on stdout (workloads:upsert /
	// workloads:remove, plus its startup "ready"). The broker uses it to
	// relay peer-origin workload events to subscribed clients.
	onNotify func(method string, params json.RawMessage)

	// relayDir is the broker's LAN directory. When the manager sends a
	// discovery:subscribe, the broker subscribes it here and pushes
	// discovery:nodes snapshots down via Forward for the manager's peer set. nil
	// when the relay isn't wired.
	relayDir *relay.Directory
	subMu    sync.Mutex
	subID    int
}

// SetLogLevel forwards an already-validated log level to the manager as a
// log/set-level JSON-RPC notification over its stdin. The manager applies
// it via the shared applog stdin handler; it writes no response because we
// send a notification, not an id-bearing request.
func (m *workloadManagerProcess) SetLogLevel(level string) error {
	return writeSetLevelFrame(&m.stdinMu, m.stdin, level)
}

// Done implements supervisedHandle: the returned channel closes once the
// workload-manager process has exited (cmd.Wait returned).
func (m *workloadManagerProcess) Done() <-chan struct{} { return m.done }

// Forward writes a JSON-RPC notification to the manager's stdin. The broker
// uses it to hand local-origin workload lifecycle / removal frames to the
// manager for cluster broadcast. Writes are serialized against Stop()'s
// stdin close.
func (m *workloadManagerProcess) Forward(method string, params json.RawMessage) error {
	data, err := json.Marshal(&Message{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	m.stdinMu.Lock()
	defer m.stdinMu.Unlock()
	_, err = m.stdin.Write(data)
	return err
}

// startWorkloadManager spawns nvpair-workload-manager with the broker's
// current log level, hides the console window on Windows, and consumes its
// stdout so the broker can relay the manager's outbound notifications.
// onNotify (may be nil) is invoked for every notification the manager
// emits. The manager's stderr is plumbed to the broker's stderr unmodified
// so its [nvpair-workload-manager] applog prefix interleaves with the broker's
// own lines, the same treatment the other workers get.
func startWorkloadManager(binaryPath, logLevel string, relayDir *relay.Directory, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*workloadManagerProcess, error) {
	cmd := exec.Command(binaryPath, append([]string{"--log-level", logLevel}, extraArgs...)...)
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

	wm := &workloadManagerProcess{
		cmd:      cmd,
		stdin:    stdin,
		done:     make(chan struct{}),
		onNotify: onNotify,
		relayDir: relayDir,
	}

	go wm.readMessages(stdout)
	go func() {
		_ = cmd.Wait()
		wm.subMu.Lock()
		if wm.subID != 0 && wm.relayDir != nil {
			wm.relayDir.Unsubscribe(wm.subID)
			wm.subID = 0
		}
		wm.subMu.Unlock()
		close(wm.done)
	}()

	return wm, nil
}

// readMessages consumes newline-delimited JSON-RPC frames from the
// manager's stdout and forwards every notification to onNotify. The
// manager's local interface is notification-only, so any id-bearing frame
// is unexpected and dropped. It runs until stdout closes (manager exit).
func (m *workloadManagerProcess) readMessages(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	// Workload payloads can be sizeable; match the proxy reader's generous
	// line cap so we never drop a frame.
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		var msg Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			slog.Warn("workload-manager emitted invalid JSON", "err", err, "raw", scanner.Text())
			continue
		}
		if msg.JSONRPC != "2.0" || msg.Method == "" {
			continue
		}
		// The manager subscribes upward for its peer set: wire it to
		// the relay directory and push events down its stdin, rather than
		// relaying this as a client-facing event.
		if msg.Method == noderec.MethodSubscribe {
			m.handleSubscribe(msg.Params)
			continue
		}
		if m.onNotify != nil {
			m.onNotify(msg.Method, msg.Params)
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("workload-manager stdout reader error", "err", err)
	}
	slog.Info("workload-manager stdout closed")
}

// handleSubscribe wires the manager's discovery:subscribe into the relay
// directory: it registers a subscriber whose Send pushes a discovery:nodes
// snapshot down the manager's stdin (via Forward), then sends the initial
// snapshot so the manager's peer set is populated immediately. A re-subscribe
// drops the prior registration first so it isn't double-fed.
func (m *workloadManagerProcess) handleSubscribe(params json.RawMessage) {
	if m.relayDir == nil {
		return
	}
	send := func(nodes []noderec.DirectoryNode) {
		data, err := json.Marshal(noderec.GetNodesResult{Nodes: nodes})
		if err != nil {
			return
		}
		if err := m.Forward(noderec.NotifyNodes, data); err != nil {
			slog.Debug("failed to push node snapshot to workload-manager", "err", err)
		}
	}
	m.subMu.Lock()
	if m.subID != 0 {
		m.relayDir.Unsubscribe(m.subID)
	}
	id, sub, err := subscribeRelay(m.relayDir, params, send)
	m.subID = id
	m.subMu.Unlock()
	if err != nil {
		slog.Warn("workload-manager sent invalid discovery:subscribe", "err", err)
		return
	}
	m.relayDir.Deliver(sub)
}

// Stop signals the manager to exit by closing its stdin, which the manager
// observes as EOF (spec 12) and treats as its shutdown signal, and waits for it
// to exit, escalating if it does not — see waitForStdinClose. Safe to call
// multiple times.
func (m *workloadManagerProcess) Stop() {
	waitForStdinClose("workload-manager", m.cmd, m.stdin, m.done)
}
