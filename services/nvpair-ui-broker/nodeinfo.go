// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

// maxNodeInfoLine caps one stdout frame from node-info. Its frames are a handful
// of addresses or a control-request response, so this is generous; the cap only
// exists so a wedged child cannot grow the broker's buffer without bound.
const maxNodeInfoLine = 64 * 1024

// nodeInfoProcess is the broker's handle on its child nvpair-node-info.
// Unlike the scanner, node-info is a *server* (HTTP node-info endpoint +
// mDNS advertiser), not an event source: the broker doesn't consume its
// stdout into the discovery store. The broker spawns it so the broker's
// own host advertises
// its GPU / CPU / memory inventory on the network, where the scanner
// (also spawned by the broker) and remote peers can then discover it.
//
// The handle owns the OS process, the stdin pipe used to signal clean
// shutdown (via EOF) and to forward runtime control frames
// (log/set-level), and a done channel that closes after cmd.Wait returns.
type nodeInfoProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdinMu serializes log-level forwarding writes against the Close()
	// in Stop() (this handle stays notify-only, not on jsonrpc.Peer).
	stdinMu sync.Mutex
	done    chan struct{}
}

// SetLogLevel forwards an already-validated log level to node-info as a
// log/set-level JSON-RPC notification over its stdin. node-info applies it
// via the shared applog stdin handler; a notification rather than an id-bearing
// request, so no response comes back on the stdout the drain below reads.
func (n *nodeInfoProcess) SetLogLevel(level string) error {
	return writeSetLevelFrame(&n.stdinMu, n.stdin, level)
}

// SetClusterIdentity tells node-info the cluster principal this node currently
// holds, so /v1/node-info can report it. node-info is spawned without a cluster
// dir (see spawnNodeInfo) and so cannot read membership for itself; this is the
// only thing that tells it. Sent on spawn and on every membership change, since
// an empty principal — a departure — is exactly the value peers need in order to
// stop treating this node as unavailable for pairing.
func (n *nodeInfoProcess) SetClusterIdentity(clusterUUID string) error {
	return writeClusterIdentityFrame(&n.stdinMu, n.stdin, clusterUUID)
}

// Done implements supervisedHandle: the returned channel closes once the
// node-info process has exited (cmd.Wait returned).
func (n *nodeInfoProcess) Done() <-chan struct{} { return n.done }

// startNodeInfo spawns nvpair-node-info with the given binary path, forwards
// the broker's current log level via --log-level, hides the console
// window on Windows, and reads its stdout for the notifications it emits.
// node-info's stderr is plumbed to the broker's stderr unmodified so its
// [nvpair-node-info] applog prefix interleaves with the broker's own lines on a
// single stream — the same treatment the scanner gets.
//
// onNotify receives each notification node-info emits, on the reader goroutine.
func startNodeInfo(binaryPath, logLevel string, onNotify func(method string, params json.RawMessage), extraArgs ...string) (*nodeInfoProcess, error) {
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

	np := &nodeInfoProcess{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}

	// Read stdout rather than discarding it: node-info reports the local
	// addresses peers have reached it on, and an unattended read end would also
	// risk wedging the child on a full pipe buffer.
	go drainNodeInfoStdout(stdout, onNotify)
	go func() {
		_ = cmd.Wait()
		close(np.done)
	}()

	return np, nil
}

// drainNodeInfoStdout consumes node-info's newline-delimited stdout until the
// pipe goes away, handing every notification frame to onNotify. Frames that are
// not notifications (a control-request response) carry no method and are skipped,
// as is anything that isn't JSON.
//
// This must never stop reading while the child lives: node-info writes its
// observed-address reports here, and an unread stdout pipe fills up and blocks
// the child mid-write, which silently ends address reporting for the rest of the
// session. bufio.Scanner cannot be used for that reason — it fails permanently on
// a frame past its limit — so an oversized frame is dropped on its own and the
// next one is read normally.
func drainNodeInfoStdout(stdout io.Reader, onNotify func(method string, params json.RawMessage)) {
	reader := bufio.NewReaderSize(stdout, maxNodeInfoLine)
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			slog.Warn("skipping oversized node-info stdout frame", "limit", maxNodeInfoLine)
			if err := discardToFrameEnd(reader); err != nil {
				logNodeInfoDrainEnd(err)
				return
			}
			continue
		}
		// A trailing read error still carries whatever preceded it: a final frame
		// written without its newline before the child exited is real output.
		if len(line) > 0 {
			handleNodeInfoFrame(line, onNotify)
		}
		if err != nil {
			logNodeInfoDrainEnd(err)
			return
		}
	}
}

// discardToFrameEnd drops bytes through the end of the oversized frame, so the
// next read starts on a frame boundary instead of mid-JSON.
func discardToFrameEnd(reader *bufio.Reader) error {
	for {
		if _, err := reader.ReadSlice('\n'); !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

// handleNodeInfoFrame decodes one frame. json.Unmarshal copies Params out, so
// nothing retains reader's slice past the next read.
func handleNodeInfoFrame(line []byte, onNotify func(method string, params json.RawMessage)) {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.Method == "" {
		return
	}
	if onNotify != nil {
		onNotify(msg.Method, msg.Params)
	}
}

// logNodeInfoDrainEnd separates the expected end of the stream — the child
// exited or Stop closed the pipe — from a read failure, which ends
// observed-address delivery and is worth a warning.
func logNodeInfoDrainEnd(err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		slog.Debug("node-info stdout closed", "err", err)
		return
	}
	slog.Warn("node-info stdout read failed; stopped draining", "err", err)
}

// Stop signals node-info to exit by closing its stdin, which node-info observes
// as EOF (via applog.StdinRPC) and treats as its shutdown signal, and waits for
// it to exit, escalating if it does not — see waitForStdinClose. Safe to call
// multiple times.
func (n *nodeInfoProcess) Stop() {
	waitForStdinClose("node-info", n.cmd, n.stdin, n.done)
}
