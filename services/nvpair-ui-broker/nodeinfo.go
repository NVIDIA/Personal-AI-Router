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
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// maxNodeInfoLine caps inventory snapshots, observed addresses and control
// responses so a wedged child cannot grow the broker's buffer without bound.
const maxNodeInfoLine = 64 * 1024

// nodeInfoProcess is the broker's handle on its child nvpair-node-info.
// It serves public hardware inventory and publishes owned hardware snapshots
// over stdout. The broker relays those snapshots to its local scanner.
//
// The handle owns the OS process, the stdin pipe used to signal clean
// shutdown (via EOF) and to forward runtime control frames
// (logging and peer metadata), and a done channel that closes after
// cmd.Wait returns.
type nodeInfoProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stdinMu keeps control frames from interleaving on the pipe. Stop closes
	// stdin without this lock so it can interrupt a blocked write.
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

type localInfoObservation struct {
	info       noderec.NodeInfo
	receivedAt time.Time
}

// acceptLocalInfo caches an owned-worker snapshot matching this host and queues
// it for a running scanner. Receipt time is retained for age-preserving replay.
func (b *Broker) acceptLocalInfo(params json.RawMessage) {
	receivedAt := time.Now()
	var snapshot noderec.LocalNodeInfoParams
	if err := json.Unmarshal(params, &snapshot); err != nil {
		slog.Warn("bad owned node-info snapshot", "err", err)
		return
	}
	if b.nodeID == "" || snapshot.Info.HostUUID != b.nodeID {
		slog.Warn("owned node-info snapshot has an unexpected host identity")
		return
	}
	observation := localInfoObservation{info: snapshot.Info, receivedAt: receivedAt}
	b.localInfoMu.Lock()
	b.localInfo = &observation
	b.localInfoMu.Unlock()
	if scanner := b.getScanner(); scanner != nil {
		scanner.reportLocalInfo(observation)
	}
}

// replayLocalInfo queues the cached snapshot for a restarted scanner, retaining
// its original receipt time so the relay includes time spent in the cache.
func (b *Broker) replayLocalInfo(scanner *scannerProcess) {
	b.localInfoMu.Lock()
	defer b.localInfoMu.Unlock()
	if b.localInfo != nil {
		scanner.reportLocalInfo(*b.localInfo)
	}
}

// updateLocalPeerMetadata projects this host's owned registry record into peer
// metadata, caching it for restart replay and forwarding changes to node-info.
func (b *Broker) updateLocalPeerMetadata(node noderec.DirectoryNode) {
	if node.IdentityVerification != noderec.IdentityLocal || node.HostUUID != b.nodeID {
		return
	}
	// Copy mutable fields so the writer can use this snapshot after releasing
	// the cache lock, even if scanner supplies another registry update.
	services := make(map[noderec.ServiceKey]int, len(node.Services))
	for key, status := range node.Services {
		services[key] = status.Port
	}
	metadata := noderec.PeerMetadataParams{
		HostUUID: node.HostUUID, ClusterUUID: node.ClusterUUID,
		Peer: noderec.PeerMetadata{Name: node.Name, Addresses: append([]string(nil), node.CandidateIPs()...), Services: services},
	}
	b.peerMetadataMu.Lock()
	previous := b.localPeerMetadata
	changed := previous == nil || previous.HostUUID != metadata.HostUUID ||
		previous.ClusterUUID != metadata.ClusterUUID || previous.Peer.Name != metadata.Peer.Name ||
		!slices.Equal(previous.Peer.Addresses, metadata.Peer.Addresses) ||
		!maps.Equal(previous.Peer.Services, metadata.Peer.Services)
	b.localPeerMetadata = &metadata
	b.peerMetadataMu.Unlock()
	if changed {
		b.pushPeerMetadata()
	}
}

// pushPeerMetadata queues the latest registry snapshot. One sender handles all
// updates, with at most one pending update while the child's stdin is blocked.
func (b *Broker) pushPeerMetadata() {
	if b.getNodeInfo() == nil {
		return
	}
	b.peerMetadataMu.Lock()
	b.peerMetadataPending = true
	if b.peerMetadataSending {
		b.peerMetadataMu.Unlock()
		return
	}
	b.peerMetadataSending = true
	b.peerMetadataMu.Unlock()
	go b.drainPeerMetadata()
}

// drainPeerMetadata exits when caught up. Restart replay queues another send;
// closing a stopped worker's stdin releases any write still using that worker.
func (b *Broker) drainPeerMetadata() {
	for {
		b.peerMetadataMu.Lock()
		if !b.peerMetadataPending {
			b.peerMetadataSending = false
			b.peerMetadataMu.Unlock()
			return
		}
		b.peerMetadataPending = false
		b.peerMetadataMu.Unlock()
		b.sendPeerMetadata()
	}
}

// sendPeerMetadata reads the cache after taking the current worker's pipe lock,
// then releases the cache lock before I/O so registry updates never wait for it.
func (b *Broker) sendPeerMetadata() {
	process := b.getNodeInfo()
	if process == nil {
		return
	}
	process.stdinMu.Lock()
	defer process.stdinMu.Unlock()
	b.peerMetadataMu.Lock()
	metadata := b.localPeerMetadata
	b.peerMetadataMu.Unlock()
	if metadata == nil {
		return
	}
	params, err := json.Marshal(metadata)
	if err != nil {
		slog.Warn("failed to encode local peer metadata", "err", err)
		return
	}
	frame := jsonrpc.Message{JSONRPC: "2.0", Method: noderec.MethodSetPeerMetadata, Params: params}
	if err := json.NewEncoder(process.stdin).Encode(frame); err != nil {
		slog.Warn("failed to push local peer metadata", "err", err)
	}
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
